package exchange

import (
	"github.com/wencaiwulue/kubevpn/util"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const VPN = "vpn"

func RemoveContainer(spec *v1.PodSpec) {
	for i := 0; i < len(spec.Containers); i++ {
		if spec.Containers[i].Name == VPN {
			spec.Containers = append(spec.Containers[:i], spec.Containers[i+1:]...)
		}
	}
}

func AddContainer(spec *v1.PodSpec, c util.PodRouteConfig) {
	// remove vpn container if already exist
	for i := 0; i < len(spec.Containers); i++ {
		if spec.Containers[i].Name == VPN {
			spec.Containers = append(spec.Containers[:i], spec.Containers[i+1:]...)
		}
	}
	t := true
	zero := int64(0)
	spec.Containers = append(spec.Containers, v1.Container{
		Name:    VPN,
		Image:   "naison/kubevpn:v2",
		Command: []string{"/bin/sh", "-c"},
		Args: []string{
			"sysctl net.ipv4.ip_forward=1;" +
				"iptables -F;" +
				"iptables -P INPUT ACCEPT;" +
				"iptables -P FORWARD ACCEPT;" +
				"iptables -t nat -A PREROUTING ! -p icmp -j DNAT --to " + c.LocalTunIP + ";" +
				"iptables -t nat -A POSTROUTING ! -p icmp -j MASQUERADE;" +
				"sysctl -w net.ipv4.conf.all.route_localnet=1;" +
				"iptables -t nat -A OUTPUT -o lo ! -p icmp -j DNAT --to-destination " + c.LocalTunIP + ";" +
				"kubevpn serve -L 'tun://0.0.0.0:8421/" + c.TrafficManagerRealIP + ":8421?net=" + c.InboundPodTunIP + "&route=" + c.Route + "' --debug=true",
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
		ImagePullPolicy: v1.PullAlways,
	})
	if len(spec.PriorityClassName) == 0 {
		spec.PriorityClassName = "system-cluster-critical"
	}
}
