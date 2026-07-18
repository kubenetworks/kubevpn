package inject

import (
	"fmt"

	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

var sidecarNames = sets.New[string](config.ContainerSidecarEnvoyProxy, config.ContainerSidecarVPN)

// RemoveContainers removes KubeVPN sidecar containers (VPN, envoy-proxy) from the pod spec.
func RemoveContainers(spec *v1.PodSpec) {
	for i := 0; i < len(spec.Containers); i++ {
		if sidecarNames.Has(spec.Containers[i].Name) {
			spec.Containers = append(spec.Containers[:i], spec.Containers[i+1:]...)
			i--
		}
	}
}

func defaultResources() v1.ResourceRequirements {
	return v1.ResourceRequirements{
		Requests: map[v1.ResourceName]resource.Quantity{
			v1.ResourceCPU:    resource.MustParse("128m"),
			v1.ResourceMemory: resource.MustParse("128Mi"),
		},
		Limits: map[v1.ResourceName]resource.Quantity{
			v1.ResourceCPU:    resource.MustParse("256m"),
			v1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
}

func privilegedSecurityContext() *v1.SecurityContext {
	return &v1.SecurityContext{
		Capabilities: &v1.Capabilities{
			Add: []v1.Capability{"NET_ADMIN", "NET_RAW"},
		},
		RunAsUser:  ptr.To[int64](0),
		RunAsGroup: ptr.To[int64](0),
		// The VPN sidecar writes /proc/sys/net/ipv4/ip_forward and related
		// sysctls at startup, which requires a privileged container — NET_ADMIN
		// alone does not grant write access to the /proc/sys network knobs.
		Privileged: ptr.To(true),
	}
}

func trafficManagerAddr(managerNamespace string) string {
	return fmt.Sprintf("%s.%s", config.ConfigMapPodTrafficManager, managerNamespace)
}

func commonEnvVars(managerNamespace string) []v1.EnvVar {
	return []v1.EnvVar{
		{Name: "CIDR4", Value: config.CIDR.String()},
		{Name: "CIDR6", Value: config.CIDR6.String()},
		{Name: "TrafficManagerService", Value: trafficManagerAddr(managerNamespace)},
		{
			Name: config.EnvPodNamespace,
			ValueFrom: &v1.EnvVarSource{
				FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
			},
		},
		{
			Name: config.EnvPodName,
			ValueFrom: &v1.EnvVarSource{
				FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.name"},
			},
		},
	}
}

func tlsEnvVars(secret *v1.Secret) []v1.EnvVar {
	return []v1.EnvVar{
		{Name: config.TLSServerName, Value: string(secret.Data[config.TLSServerName])},
		{Name: config.TLSCertKey, Value: string(secret.Data[config.TLSCertKey])},
		{Name: config.TLSPrivateKeyKey, Value: string(secret.Data[config.TLSPrivateKeyKey])},
	}
}

func newEnvoyContainer(ns, nodeID string, ipv6 bool, managerNamespace, image string, configIPv6, configIPv4 []byte) v1.Container {
	addr := trafficManagerAddr(managerNamespace)
	configYaml := renderEnvoyConfig(string(configIPv4), addr)
	if ipv6 {
		configYaml = renderEnvoyConfig(string(configIPv6), addr)
	}
	return v1.Container{
		Name:  config.ContainerSidecarEnvoyProxy,
		Image: image,
		Command: []string{
			"envoy",
			"-l",
			util.If(config.Debug, log.DebugLevel, log.InfoLevel).String(),
			"--base-id", "1",
			"--service-node", util.GenEnvoyUID(ns, nodeID),
			"--service-cluster", util.GenEnvoyUID(ns, nodeID),
			"--config-yaml",
		},
		Args:            []string{configYaml},
		Resources:       defaultResources(),
		ImagePullPolicy: v1.PullIfNotPresent,
	}
}

// AddVPNAndEnvoyContainer adds VPN and Envoy sidecar containers for mesh mode.
func AddVPNAndEnvoyContainer(spec *v1.PodTemplateSpec, ns, nodeID string, ipv6 bool, managerNamespace string, secret *v1.Secret, image string) {
	RemoveContainers(&spec.Spec)
	spec.Spec.ServiceAccountName = config.ConfigMapPodTrafficManager

	var envs []v1.EnvVar
	envs = append(envs, commonEnvVars(managerNamespace)...)
	envs = append(envs, tlsEnvVars(secret)...)

	spec.Spec.Containers = append(spec.Spec.Containers, v1.Container{
		Name:    config.ContainerSidecarVPN,
		Image:   image,
		Command: []string{"/bin/bash", "-c"},
		Args: []string{fmt.Sprintf(`
echo 1 > /proc/sys/net/ipv4/ip_forward
echo 0 > /proc/sys/net/ipv6/conf/all/disable_ipv6
echo 1 > /proc/sys/net/ipv6/conf/all/forwarding
echo 0 > /proc/sys/net/ipv4/tcp_timestamps
for b in iptables-nft iptables-legacy; do "$b" -t nat -L &>/dev/null && update-alternatives --set iptables /usr/sbin/$b && update-alternatives --set ip6tables /usr/sbin/${b/iptables/ip6tables} && break; done
iptables -P INPUT ACCEPT
ip6tables -P INPUT ACCEPT
iptables -P FORWARD ACCEPT
ip6tables -P FORWARD ACCEPT
iptables -t nat -A PREROUTING -p tcp ! -s 127.0.0.1 ! -d ${CIDR4} -j DNAT --to :%d
ip6tables -t nat -A PREROUTING -p tcp ! -s 0:0:0:0:0:0:0:1 ! -d ${CIDR6} -j DNAT --to :%d
kubevpn server -l "tun:/tcp://${TrafficManagerService}:%d?route=${CIDR4}"`, config.PortEnvoyInbound, config.PortEnvoyInbound, config.PortTCP),
		},
		Env:             envs,
		Resources:       defaultResources(),
		ImagePullPolicy: v1.PullIfNotPresent,
		SecurityContext: privilegedSecurityContext(),
	})

	spec.Spec.Containers = append(spec.Spec.Containers,
		newEnvoyContainer(ns, nodeID, ipv6, managerNamespace, image, envoyConfig, envoyConfigIPv4))
}

// AddEnvoyAndSSHContainer adds SSH and Envoy sidecar containers for fargate/service mode.
func AddEnvoyAndSSHContainer(spec *v1.PodTemplateSpec, ns, nodeID string, ipv6 bool, managerNamespace string, image string) {
	RemoveContainers(&spec.Spec)

	spec.Spec.Containers = append(spec.Spec.Containers, v1.Container{
		Name:            config.ContainerSidecarVPN,
		Image:           image,
		Command:         []string{"kubevpn"},
		Args:            []string{"server", fmt.Sprintf("-l ssh://:%d", config.PortSSH), fmt.Sprintf("-l udpbridge://:%d", config.PortUDPBridge)},
		Resources:       defaultResources(),
		ImagePullPolicy: v1.PullIfNotPresent,
		SecurityContext: &v1.SecurityContext{},
	})

	spec.Spec.Containers = append(spec.Spec.Containers,
		newEnvoyContainer(ns, nodeID, ipv6, managerNamespace, image, envoyConfigFargate, envoyConfigIPv4Fargate))
}
