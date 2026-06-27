package inject

import (
	"context"
	"fmt"
	"net/netip"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type fargateInjector struct {
	opts InjectOptions
}

func (f *fargateInjector) Inject(ctx context.Context) error {
	o := f.opts
	plog.G(ctx).Debugf("Injecting fargate (SSH+Envoy) sidecar into %s/%s", o.Controller.Mapping.Resource.Resource, o.Controller.Name)
	u := o.Controller.Object.(*unstructured.Unstructured)

	templateSpec, path, err := util.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}

	localTunIPv4 := "127.0.0.1"
	localTunIPv6 := netip.IPv6Loopback().String()
	ports, portmap := collectFargatePorts(templateSpec, o.PortMaps)
	port := controlplane.ConvertContainerPort(ports...)
	containerPort2EnvoyListenerPort := make(map[int32]int32)
	for i := range len(port) {
		randomPort, _ := util.GetAvailableTCPPort()
		port[i].EnvoyListenerPort = int32(randomPort)
		containerPort2EnvoyListenerPort[port[i].ContainerPort] = int32(randomPort)
	}

	err = addEnvoyConfig(ctx, o.Clientset.CoreV1().ConfigMaps(o.ManagerNamespace), o.Controller.Namespace, o.NodeID, localTunIPv4, localTunIPv6, o.Headers, port, portmap, true)
	if err != nil {
		plog.G(ctx).Errorf("Failed to add envoy config: %v", err)
		return err
	}

	if alreadyInjected(templateSpec) {
		workload := fmt.Sprintf("%s/%s", o.Controller.Mapping.Resource.Resource, o.Controller.Name)
		plog.G(ctx).Infof("Workload %s/%s has already been injected with sidecar", o.Controller.Namespace, workload)
		return nil
	}

	enableIPv6, _ := util.DetectPodSupportIPv6(ctx, o.Factory, o.ManagerNamespace)
	AddEnvoyAndSSHContainer(templateSpec, o.Controller.Namespace, o.NodeID, enableIPv6, o.ManagerNamespace, o.Image)

	err = patchWorkload(ctx, o.Factory, o.Controller, templateSpec, path)
	if err != nil {
		return err
	}

	return ModifyServiceTargetPort(ctx, o.Clientset, o.Controller.Namespace, o.Object.Name, containerPort2EnvoyListenerPort)
}

// ModifyServiceTargetPort updates a Service's target ports to point to envoy listener ports.
func ModifyServiceTargetPort(ctx context.Context, clientset kubernetes.Interface, namespace string, name string, m map[int32]int32) error {
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

// collectFargatePorts collects container ports and generates the fargate port mapping.
// Each container port gets a random envoyRulePort. The local listening port defaults
// to the containerPort but is overridden by the host port from portMaps if specified.
// Returns: (raw container ports, portmap: containerPort → "envoyRulePort:localPort")
func collectFargatePorts(templateSpec *v1.PodTemplateSpec, portMaps []string) ([]v1.ContainerPort, map[int32]string) {
	ports := gatherContainerPorts(templateSpec, portMaps)

	// Build a lookup for portMap overrides: containerPort → hostPort
	hostPortOverride := make(map[int32]int32)
	for _, pm := range portMaps {
		port := util.ParsePort(pm)
		if port.ContainerPort != 0 {
			hostPortOverride[port.ContainerPort] = port.HostPort
		}
	}

	portmap := make(map[int32]string)
	for _, port := range ports {
		envoyRulePort, _ := util.GetAvailableTCPPort()
		localPort := port.ContainerPort
		if hp, ok := hostPortOverride[port.ContainerPort]; ok {
			localPort = hp
		}
		portmap[port.ContainerPort] = fmt.Sprintf("%d:%d", envoyRulePort, localPort)
	}
	return ports, portmap
}
