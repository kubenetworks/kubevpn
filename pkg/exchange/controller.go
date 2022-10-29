package exchange

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func RemoveContainer(spec *corev1.PodSpec) {
	for i := 0; i < len(spec.Containers); i++ {
		if spec.Containers[i].Name == config.SidecarVPN {
			spec.Containers = append(spec.Containers[:i], spec.Containers[i+1:]...)
			i--
		}
	}
}

func AddContainer(spec *corev1.PodSpec, c util.PodRouteConfig) {
	// remove vpn container if already exist
	RemoveContainer(spec)
	t := true
	zero := int64(0)
	spec.Containers = append(spec.Containers, corev1.Container{
		Name:  config.SidecarVPN,
		Image: config.ImageServer,
		Env: []corev1.EnvVar{
			{
				Name:  "LocalTunIP",
				Value: c.LocalTunIP,
			},
			{
				Name:  "TrafficManagerRealIP",
				Value: c.TrafficManagerRealIP,
			},
			{
				Name:  "InboundPodTunIP",
				Value: c.InboundPodTunIP,
			},
			{
				Name:  "Route",
				Value: c.Route,
			},
		},
		Command: []string{"/bin/sh", "-c"},
		Args: []string{`
sysctl net.ipv4.ip_forward=1
sysctl -w net.ipv4.conf.all.route_localnet=1
update-alternatives --set iptables /usr/sbin/iptables-legacy
iptables -F
iptables -P INPUT ACCEPT
iptables -P FORWARD ACCEPT
iptables -t nat -A PREROUTING ! -p icmp -j DNAT --to $(LocalTunIP)
iptables -t nat -A POSTROUTING ! -p icmp -j MASQUERADE
iptables -t nat -A OUTPUT -o lo ! -p icmp -j DNAT --to-destination $(LocalTunIP)
kubevpn serve -L 'tun://0.0.0.0:8421/$(TrafficManagerRealIP):8422?net=$(InboundPodTunIP)&route=$(Route)' --debug=true`,
		},
		SecurityContext: &corev1.SecurityContext{
			Capabilities: &corev1.Capabilities{
				Add: []corev1.Capability{
					"NET_ADMIN",
					//"SYS_MODULE",
				},
			},
			RunAsUser:  &zero,
			Privileged: &t,
		},
		Resources: corev1.ResourceRequirements{
			Requests: map[corev1.ResourceName]resource.Quantity{
				corev1.ResourceCPU:    resource.MustParse("128m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Limits: map[corev1.ResourceName]resource.Quantity{
				corev1.ResourceCPU:    resource.MustParse("256m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
		ImagePullPolicy: corev1.PullIfNotPresent,
	})
	if len(spec.PriorityClassName) == 0 {
		spec.PriorityClassName = "system-cluster-critical"
	}
}
