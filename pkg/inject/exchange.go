package inject

import (
	"fmt"

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

func AddContainer(spec *corev1.PodSpec, c util.PodRouteConfig, managerNamespace string, secret *corev1.Secret, image string) {
	// remove vpn container if already exist
	RemoveContainer(spec)
	spec.Containers = append(spec.Containers, corev1.Container{
		Name:  config.ContainerSidecarVPN,
		Image: image,
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
				Value: fmt.Sprintf("%s.%s", config.ConfigMapPodTrafficManager, managerNamespace),
			},
			{
				Name: config.EnvPodNamespace,
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "metadata.namespace",
					},
				},
			},
			{
				Name: config.EnvPodName,
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "metadata.name",
					},
				},
			},
			{
				Name:  config.TLSServerName,
				Value: string(secret.Data[config.TLSServerName]),
			},
			{
				Name:  config.TLSCertKey,
				Value: string(secret.Data[config.TLSCertKey]),
			},
			{
				Name:  config.TLSPrivateKeyKey,
				Value: string(secret.Data[config.TLSPrivateKeyKey]),
			},
		},
		Command: []string{"/bin/sh", "-c"},
		// https://www.netfilter.org/documentation/HOWTO/NAT-HOWTO-6.html#ss6.2
		// for curl -g -6 [2001:2::999a]:9080/health or curl 127.0.0.1:9080/health hit local PC
		// output chain
		// iptables -t nat -A OUTPUT -o lo ! -p icmp -j DNAT --to-destination ${LocalTunIPv4}
		// ip6tables -t nat -A OUTPUT -o lo ! -p icmp -j DNAT --to-destination ${LocalTunIPv6}
		Args: []string{`
echo 1 > /proc/sys/net/ipv4/ip_forward
echo 0 > /proc/sys/net/ipv6/conf/all/disable_ipv6
echo 1 > /proc/sys/net/ipv6/conf/all/forwarding
echo 1 > /proc/sys/net/ipv4/conf/all/route_localnet
update-alternatives --set iptables /usr/sbin/iptables-legacy
iptables -P INPUT ACCEPT
ip6tables -P INPUT ACCEPT
iptables -P FORWARD ACCEPT
ip6tables -P FORWARD ACCEPT
iptables -t nat -A PREROUTING ! -p icmp -j DNAT --to ${LocalTunIPv4}
ip6tables -t nat -A PREROUTING ! -p icmp -j DNAT --to ${LocalTunIPv6}
iptables -t nat -A POSTROUTING ! -p icmp -j MASQUERADE
ip6tables -t nat -A POSTROUTING ! -p icmp -j MASQUERADE
kubevpn server -l "tun:/127.0.0.1:8422?net=${TunIPv4}&net6=${TunIPv6}&route=${CIDR4}" -f "tcp://${TrafficManagerService}:10801"`,
		},
		SecurityContext: &corev1.SecurityContext{
			Capabilities: &corev1.Capabilities{
				Add: []corev1.Capability{
					"NET_ADMIN",
					//"SYS_MODULE",
				},
			},
			RunAsUser:  pointer.Int64(0),
			RunAsGroup: pointer.Int64(0),
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
}
