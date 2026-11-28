package inject

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"

	clusterworkload "github.com/wencaiwulue/kubevpn/v2/pkg/cluster/workload"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// Injector writes node-boundary attachment state for a workload or Service.
type Injector interface {
	Inject(ctx context.Context) error
}

type InjectOptions struct {
	Clientset        kubernetes.Interface
	ManagerNamespace string
	NodeID           string
	Object           *resource.Info
	Controller       *resource.Info
	LocalTunIPv4     string
	LocalTunIPv6     string
	Headers          map[string]string
	PortMaps         []string
	OwnerID          string
	SyncTarget       *controlplane.SyncTarget
	ServiceName      string
}

func isServiceTarget(object *resource.Info) bool {
	if object == nil || object.Mapping == nil {
		return false
	}
	return clusterworkload.IsK8sService(object)
}

// NewInjector returns the single node-boundary attachment implementation.
func NewInjector(opts InjectOptions) Injector {
	return &nodeAgentInjector{opts: opts}
}

// gatherContainerPorts collects declared container ports and any additional
// ports supplied through portMaps.
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
