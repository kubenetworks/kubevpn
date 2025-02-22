package inject

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8sjson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/sets"
	pkgresource "k8s.io/cli-runtime/pkg/resource"
	runtimeresource "k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// InjectEnvoySidecar patch a sidecar, using iptables to do port-forward let this pod decide should go to 233.254.254.100 or request to 127.0.0.1
// https://istio.io/latest/docs/ops/deployment/requirements/#ports-used-by-istio
func InjectEnvoySidecar(ctx context.Context, f cmdutil.Factory, clientset *kubernetes.Clientset, namespace, workload string, object *runtimeresource.Info, headers map[string]string, portMap []string) (err error) {
	u := object.Object.(*unstructured.Unstructured)
	var templateSpec *v1.PodTemplateSpec
	var path []string
	templateSpec, path, err = util.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}

	nodeID := fmt.Sprintf("%s.%s", object.Mapping.Resource.GroupResource().String(), object.Name)

	c := util.PodRouteConfig{LocalTunIPv4: "127.0.0.1", LocalTunIPv6: netip.IPv6Loopback().String()}
	ports, portmap := GetPort(templateSpec, portMap)
	port := controlplane.ConvertContainerPort(ports...)
	var containerPort2EnvoyListenerPort = make(map[int32]int32)
	for i := range len(port) {
		randomPort, _ := util.GetAvailableTCPPortOrDie()
		port[i].EnvoyListenerPort = int32(randomPort)
		containerPort2EnvoyListenerPort[port[i].ContainerPort] = int32(randomPort)
	}
	err = addEnvoyConfig(clientset.CoreV1().ConfigMaps(namespace), nodeID, c, headers, port, portmap)
	if err != nil {
		log.Errorf("Failed to add envoy config: %v", err)
		return err
	}

	// already inject container envoy-proxy, do nothing
	containerNames := sets.New[string]()
	for _, container := range templateSpec.Spec.Containers {
		containerNames.Insert(container.Name)
	}
	if containerNames.HasAll(config.ContainerSidecarVPN, config.ContainerSidecarEnvoyProxy) {
		log.Infof("Workload %s/%s has already been injected with sidecar", namespace, workload)
		return
	}

	enableIPv6, _ := util.DetectPodSupportIPv6(ctx, f, namespace)
	// (1) add mesh container
	AddEnvoyContainer(templateSpec, nodeID, enableIPv6)
	helper := pkgresource.NewHelper(object.Client, object.Mapping)
	ps := []P{
		{
			Op:    "replace",
			Path:  "/" + strings.Join(append(path, "spec"), "/"),
			Value: templateSpec.Spec,
		},
	}
	var bytes []byte
	bytes, err = k8sjson.Marshal(append(ps))
	if err != nil {
		return err
	}
	_, err = helper.Patch(object.Namespace, object.Name, types.JSONPatchType, bytes, &metav1.PatchOptions{})
	if err != nil {
		log.Errorf("Failed to patch resource: %s %s, err: %v", object.Mapping.Resource.Resource, object.Name, err)
		return err
	}
	log.Infof("Patching workload %s", workload)
	err = util.RolloutStatus(ctx, f, namespace, workload, time.Minute*60)
	if err != nil {
		return err
	}

	// 2) modify service containerPort to envoy listener port
	err = ModifyServiceTargetPort(ctx, clientset, namespace, templateSpec.Labels, containerPort2EnvoyListenerPort)
	if err != nil {
		return err
	}
	return nil
}

func ModifyServiceTargetPort(ctx context.Context, clientset *kubernetes.Clientset, namespace string, podLabels map[string]string, m map[int32]int32) error {
	// service selector == pod labels
	list, err := clientset.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}

	var svc *v1.Service
	for _, item := range list.Items {
		if labels.SelectorFromSet(item.Spec.Selector).Matches(labels.Set(podLabels)) {
			svc = &item
			break
		}
	}
	if svc == nil {
		return fmt.Errorf("can not found service with selector: %v", podLabels)
	}
	for i := range len(svc.Spec.Ports) {
		svc.Spec.Ports[i].TargetPort = intstr.FromInt32(m[svc.Spec.Ports[i].Port])
	}
	_, err = clientset.CoreV1().Services(namespace).Update(ctx, svc, metav1.UpdateOptions{})
	return err
}

func GetPort(templateSpec *v1.PodTemplateSpec, portMaps []string) ([]v1.ContainerPort, map[int32]string) {
	var ports []v1.ContainerPort
	for _, container := range templateSpec.Spec.Containers {
		ports = append(ports, container.Ports...)
	}
	var found = func(containerPort int32) bool {
		for _, port := range ports {
			if port.ContainerPort == containerPort {
				return true
			}
		}
		return false
	}
	for _, portMap := range portMaps {
		port := util.ParsePort(portMap)
		port.HostPort = 0
		if port.ContainerPort != 0 && !found(port.ContainerPort) {
			ports = append(ports, port)
		}
	}

	var portmap = make(map[int32]string)
	for _, port := range ports {
		randomPort, _ := util.GetAvailableTCPPortOrDie()
		portmap[port.ContainerPort] = fmt.Sprintf("%d:%d", randomPort, port.ContainerPort)
	}
	for _, portMap := range portMaps {
		port := util.ParsePort(portMap)
		if port.ContainerPort != 0 {
			randomPort, _ := util.GetAvailableTCPPortOrDie()
			portmap[port.ContainerPort] = fmt.Sprintf("%d:%d", randomPort, port.HostPort)
		}
	}
	return ports, portmap
}

var _ = `function EPHEMERAL_PORT() {
    UPORT=65535
    LPORT=30000
    while true; do
        CANDIDATE=$[$LPORT + ($RANDOM % ($UPORT-$LPORT))]
        (echo -n >/dev/tcp/127.0.0.1/${CANDIDATE}) >/dev/null 2>&1
        if [ $? -ne 0 ]; then
            echo $CANDIDATE
            break
        fi
    done
}`
