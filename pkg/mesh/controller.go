package mesh

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/wencaiwulue/kubevpn/config"
	"github.com/wencaiwulue/kubevpn/util"
)

func RemoveContainers(spec *v1.PodTemplateSpec) {
	for i := 0; i < len(spec.Spec.Containers); i++ {
		if sets.NewString(config.SidecarEnvoyProxy, config.SidecarVPN).Has(spec.Spec.Containers[i].Name) {
			spec.Spec.Containers = append(spec.Spec.Containers[:i], spec.Spec.Containers[i+1:]...)
			i--
		}
	}
}

func AddMeshContainer(spec *v1.PodTemplateSpec, nodeId string, c util.PodRouteConfig) {
	// remove envoy proxy containers if already exist
	RemoveContainers(spec)
	zero := int64(0)
	t := true
	spec.Spec.Containers = append(spec.Spec.Containers, v1.Container{
		Name:    config.SidecarVPN,
		Image:   config.ImageServer,
		Command: []string{"/bin/sh", "-c"},
		Args: []string{
			"sysctl net.ipv4.ip_forward=1;" +
				"iptables -F;" +
				"iptables -P INPUT ACCEPT;" +
				"iptables -P FORWARD ACCEPT;" +
				"iptables -t nat -A PREROUTING ! -p icmp ! -s 127.0.0.1 ! -d " + config.CIDR.String() + " -j DNAT --to 127.0.0.1:15006;" +
				"iptables -t nat -A POSTROUTING ! -p icmp ! -s 127.0.0.1 ! -d " + config.CIDR.String() + " -j MASQUERADE;" +
				"kubevpn serve -L 'tun:/" + c.TrafficManagerRealIP + ":8422?net=" + c.InboundPodTunIP + "&route=" + c.Route + "' --debug=true",
		},
		SecurityContext: &v1.SecurityContext{
			Capabilities: &v1.Capabilities{
				Add: []v1.Capability{
					"NET_ADMIN",
					//"SYS_MODULE",
				},
			},
			RunAsUser:  &zero,
			Privileged: &t,
		},
		Resources: v1.ResourceRequirements{
			Requests: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:    resource.MustParse("128m"),
				v1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Limits: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:    resource.MustParse("256m"),
				v1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
		ImagePullPolicy: v1.PullIfNotPresent,
	})
	spec.Spec.Containers = append(spec.Spec.Containers, v1.Container{
		Name:    config.SidecarEnvoyProxy,
		Image:   config.ImageMesh,
		Command: []string{"envoy", "-l", "debug", "--config-yaml"},
		Args: []string{
			fmt.Sprintf(s, nodeId, nodeId, c.TrafficManagerRealIP),
		},
		Resources: v1.ResourceRequirements{
			Requests: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:    resource.MustParse("128m"),
				v1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Limits: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:    resource.MustParse("256m"),
				v1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
		ImagePullPolicy: v1.PullIfNotPresent,
	})
}
