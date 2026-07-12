package inject

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

var errPodCreated = errors.New("pod created, waiting for running")

// Injector manages the lifecycle of sidecar injection for a Kubernetes workload.
type Injector interface {
	Inject(ctx context.Context) error
}

type InjectOptions struct {
	Factory          cmdutil.Factory
	Clientset        kubernetes.Interface
	ManagerNamespace string
	NodeID           string
	Object           *resource.Info
	Controller       *resource.Info
	LocalTunIPv4     string
	LocalTunIPv6     string
	Headers          map[string]string
	PortMaps         []string
	Secret           *v1.Secret
	Image            string
	OwnerID          string
}

// NewInjector returns the appropriate Injector for the given workload and options.
// - Service targets use Fargate mode (SSH + Envoy).
// - All other targets use Mesh mode (VPN + Envoy). When headers are empty,
//   envoy matches all requests (full traffic interception, equivalent to legacy VPN-only).
func NewInjector(opts InjectOptions) Injector {
	if util.IsK8sService(opts.Object) {
		return &fargateInjector{opts: opts}
	}
	return &meshInjector{opts: opts}
}

type JSONPatchOp struct {
	Op    string      `json:"op,omitempty"`
	Path  string      `json:"path,omitempty"`
	Value any `json:"value,omitempty"`
}

func patchWorkload(ctx context.Context, factory cmdutil.Factory, info *resource.Info, templateSpec *v1.PodTemplateSpec, path []string) error {
	workload := fmt.Sprintf("%s/%s", info.Mapping.Resource.Resource, info.Name)
	helper := resource.NewHelper(info.Client, info.Mapping)

	// pods without controller
	if len(path) == 0 {
		plog.G(ctx).Infof("Workload %s is not controlled by any controller", workload)
		p := &v1.Pod{ObjectMeta: templateSpec.ObjectMeta, Spec: templateSpec.Spec}
		clearPodMetadata(p)
		return recreatePod(ctx, factory, p, helper)
	}

	plog.G(ctx).Debugf("The %s is under controller management", workload)
	ops := []JSONPatchOp{{
		Op:    "replace",
		Path:  "/" + strings.Join(append(path, "spec"), "/"),
		Value: templateSpec.Spec,
	}}
	data, err := json.Marshal(ops)
	if err != nil {
		return err
	}
	_, err = helper.Patch(info.Namespace, info.Name, types.JSONPatchType, data, &metav1.PatchOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Failed to patch resource: %s %s, err: %v", info.Mapping.Resource.Resource, info.Name, err)
		return err
	}
	plog.G(ctx).Infof("Patching workload %s", workload)
	return util.RolloutStatus(ctx, factory, info.Namespace, workload)
}

// gatherContainerPorts collects all container ports from a pod template spec,
// adding any ports from portMaps that aren't already present.
// Shared by both mesh (collectPorts) and fargate (collectFargatePorts) injectors.
func gatherContainerPorts(templateSpec *v1.PodTemplateSpec, portMaps []string) []v1.ContainerPort {
	var ports []v1.ContainerPort
	for _, container := range templateSpec.Spec.Containers {
		ports = append(ports, container.Ports...)
	}
	known := func(containerPort int32) bool {
		for _, port := range ports {
			if port.ContainerPort == containerPort {
				return true
			}
		}
		return false
	}
	for _, pm := range portMaps {
		port := util.ParsePort(pm)
		port.HostPort = 0
		if port.ContainerPort != 0 && !known(port.ContainerPort) {
			ports = append(ports, port)
		}
	}
	return ports
}

func collectPorts(templateSpec *v1.PodTemplateSpec, portMaps []string) ([]controlplane.ContainerPort, map[int32]string) {
	ports := gatherContainerPorts(templateSpec, portMaps)
	envoyPorts := controlplane.ConvertContainerPort(ports...)
	portmap := make(map[int32]string)
	for _, p := range envoyPorts {
		portmap[p.ContainerPort] = fmt.Sprintf("%d", p.ContainerPort)
	}
	for _, pm := range portMaps {
		port := util.ParsePort(pm)
		if port.ContainerPort != 0 {
			portmap[port.ContainerPort] = fmt.Sprintf("%d", port.HostPort)
		}
	}
	return envoyPorts, portmap
}

const (
	// recreatePodRetrySteps is the number of attempts to recreate a pod after deletion.
	recreatePodRetrySteps = 10
	// recreatePodRetryDuration is the initial backoff before the first recreate retry.
	recreatePodRetryDuration = 50 * time.Millisecond
	// recreatePodRetryFactor is the exponential backoff multiplier between recreate retries.
	recreatePodRetryFactor = 5.0
	// recreatePodRetryJitter is the jitter fraction applied to the recreate backoff.
	recreatePodRetryJitter = 1.0
)

func recreatePod(ctx context.Context, factory cmdutil.Factory, p *v1.Pod, helper *resource.Helper) error {
	_, err := helper.DeleteWithOptions(p.Namespace, p.Name, &metav1.DeleteOptions{
		GracePeriodSeconds: ptr.To[int64](0),
	})
	if err != nil {
		plog.G(ctx).Errorf("Failed to delete resource: %s %s, ignore, err: %v", p.Namespace, p.Name, err)
	}
	err = retry.OnError(wait.Backoff{
		Steps:    recreatePodRetrySteps,
		Duration: recreatePodRetryDuration,
		Factor:   recreatePodRetryFactor,
		Jitter:   recreatePodRetryJitter,
	}, func(err error) bool {
		if !k8serrors.IsAlreadyExists(err) {
			return true
		}
		clientset, err := factory.KubernetesClientSet()
		get, err := clientset.CoreV1().Pods(p.Namespace).Get(context.Background(), p.Name, metav1.GetOptions{})
		if err != nil || get.Status.Phase != v1.PodRunning {
			return true
		}
		return false
	}, func() error {
		if _, err := helper.Create(p.Namespace, true, p); err != nil {
			return err
		}
		return errPodCreated
	})
	if err != nil {
		if k8serrors.IsAlreadyExists(err) {
			return nil
		}
		plog.G(ctx).Errorf("Failed to create resource: %s %s, err: %v", p.Namespace, p.Name, err)
		return err
	}
	return nil
}

func clearPodMetadata(pod *v1.Pod) {
	pod.SetSelfLink("")
	pod.SetGeneration(0)
	pod.SetResourceVersion("")
	pod.SetUID("")
	pod.SetDeletionTimestamp(nil)
	pod.SetManagedFields(nil)
	pod.SetOwnerReferences(nil)
}
