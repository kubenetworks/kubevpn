package exchange

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/pointer"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func RemoveContainer(spec *corev1.PodSpec) {
	for i := 0; i < len(spec.Containers); i++ {
		if spec.Containers[i].Name == config.ContainerSidecarVPN {
			spec.Containers = append(spec.Containers[:i], spec.Containers[i+1:]...)
			i--
		}
	}
}

func AddContainer(spec *corev1.PodSpec, c util.PodRouteConfig) {
	// remove vpn container if already exist
	RemoveContainer(spec)
	spec.Containers = append(spec.Containers, corev1.Container{
		Name:  config.ContainerSidecarVPN,
		Image: config.Image,
		EnvFrom: []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: config.ConfigMapPodTrafficManager,
				},
			},
		}},
		Env: []corev1.EnvVar{
			{
				Name:  "LocalTunIPv4",
				Value: c.LocalTunIPv4,
			},
			{
				Name:  "LocalTunIPv6",
				Value: c.LocalTunIPv6,
			},
			{
				Name:  config.EnvInboundPodTunIPv4,
				Value: "",
			},
			{
				Name:  config.EnvInboundPodTunIPv6,
				Value: "",
			},
			{
				Name:  "CIDR4",
				Value: config.CIDR.String(),
			},
			{
				Name:  "CIDR6",
				Value: config.CIDR6.String(),
			},
			{
				Name:  "TrafficManagerService",
				Value: config.ConfigMapPodTrafficManager,
			},
		},
		Command: []string{"/bin/sh", "-c"},
		// https://www.netfilter.org/documentation/HOWTO/NAT-HOWTO-6.html#ss6.2
		// for curl -g -6 [efff:ffff:ffff:ffff:ffff:ffff:ffff:999a]:9080/health or curl 127.0.0.1:9080/health hit local PC
		// output chain
		Args: []string{`
sysctl -w net.ipv4.ip_forward=1
sysctl -w net.ipv6.conf.all.disable_ipv6=0
sysctl -w net.ipv6.conf.all.forwarding=1
sysctl -w net.ipv4.conf.all.route_localnet=1
update-alternatives --set iptables /usr/sbin/iptables-legacy
iptables -F
ip6tables -F
iptables -P INPUT ACCEPT
ip6tables -P INPUT ACCEPT
iptables -P FORWARD ACCEPT
ip6tables -P FORWARD ACCEPT
iptables -t nat -A PREROUTING ! -p icmp -j DNAT --to ${LocalTunIPv4}
ip6tables -t nat -A PREROUTING ! -p icmp -j DNAT --to ${LocalTunIPv6}
iptables -t nat -A POSTROUTING ! -p icmp -j MASQUERADE
ip6tables -t nat -A POSTROUTING ! -p icmp -j MASQUERADE
iptables -t nat -A OUTPUT -o lo ! -p icmp -j DNAT --to-destination ${LocalTunIPv4}
ip6tables -t nat -A OUTPUT -o lo ! -p icmp -j DNAT --to-destination ${LocalTunIPv6}
kubevpn serve -L "tun:/127.0.0.1:8422?net=${TunIPv4}&route=${CIDR4}" -F "tcp://${TrafficManagerService}:10800"`,
		},
		SecurityContext: &corev1.SecurityContext{
			Capabilities: &corev1.Capabilities{
				Add: []corev1.Capability{
					"NET_ADMIN",
					//"SYS_MODULE",
				},
			},
			RunAsUser:  pointer.Int64(0),
			Privileged: pointer.Bool(true),
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
