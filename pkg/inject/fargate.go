package inject

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// InjectEnvoyAndSSH patch a sidecar, using iptables to do port-forward let this pod decide should go to 233.254.254.100 or request to 127.0.0.1
// https://istio.io/latest/docs/ops/deployment/requirements/#ports-used-by-istio
func InjectEnvoyAndSSH(ctx context.Context, nodeID string, f cmdutil.Factory, managerNamespace string, current, object *runtimeresource.Info, headers map[string]string, portMap []string, image string) (err error) {
	var clientset *kubernetes.Clientset
	clientset, err = f.KubernetesClientSet()
	if err != nil {
		return err
	}
	u := object.Object.(*unstructured.Unstructured)
	var templateSpec *v1.PodTemplateSpec
	var path []string
	templateSpec, path, err = util.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}

	c := util.PodRouteConfig{LocalTunIPv4: "127.0.0.1", LocalTunIPv6: netip.IPv6Loopback().String()}
	ports, portmap := GetPort(templateSpec, portMap)
	port := controlplane.ConvertContainerPort(ports...)
	var containerPort2EnvoyListenerPort = make(map[int32]int32)
	for i := range len(port) {
		randomPort, _ := util.GetAvailableTCPPortOrDie()
		port[i].EnvoyListenerPort = int32(randomPort)
		containerPort2EnvoyListenerPort[port[i].ContainerPort] = int32(randomPort)
	}
	err = addEnvoyConfig(clientset.CoreV1().ConfigMaps(managerNamespace), object.Namespace, nodeID, c, headers, port, portmap)
	if err != nil {
		plog.G(ctx).Errorf("Failed to add envoy config: %v", err)
		return err
	}
	workload := fmt.Sprintf("%s/%s", object.Mapping.Resource.Resource, object.Name)
	// already inject container envoy-proxy, do nothing
	containerNames := sets.New[string]()
	for _, container := range templateSpec.Spec.Containers {
		containerNames.Insert(container.Name)
	}
	if containerNames.HasAll(config.ContainerSidecarVPN, config.ContainerSidecarEnvoyProxy) {
		plog.G(ctx).Infof("Workload %s/%s has already been injected with sidecar", object.Namespace, workload)
		return
	}

	enableIPv6, _ := util.DetectPodSupportIPv6(ctx, f, managerNamespace)
	// (1) add mesh container
	AddEnvoyContainer(templateSpec, object.Namespace, nodeID, enableIPv6, managerNamespace, image)
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
		plog.G(ctx).Errorf("Failed to patch resource: %s %s, err: %v", object.Mapping.Resource.Resource, object.Name, err)
		return err
	}
	plog.G(ctx).Infof("Patching workload %s", workload)
	err = util.RolloutStatus(ctx, f, object.Namespace, workload, time.Minute*60)
	if err != nil {
		return err
	}

	if !util.IsK8sService(current) {
		return nil
	}
	// 2) modify service containerPort to envoy listener port
	err = ModifyServiceTargetPort(ctx, clientset, object.Namespace, current.Name, containerPort2EnvoyListenerPort)
	if err != nil {
		return err
	}
	return nil
}

func ModifyServiceTargetPort(ctx context.Context, clientset *kubernetes.Clientset, namespace string, name string, m map[int32]int32) error {
	svc, err := clientset.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	for i := range len(svc.Spec.Ports) {
		if p, found := m[svc.Spec.Ports[i].Port]; found {
			svc.Spec.Ports[i].TargetPort = intstr.FromInt32(p)
		} else {
			svc.Spec.Ports[i].TargetPort = intstr.FromInt32(svc.Spec.Ports[i].Port)
		}
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
