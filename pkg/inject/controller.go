package inject

import (
	"bytes"
	_ "embed"
	"fmt"
	"text/template"

	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/pointer"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

//go:embed envoy.yaml
var envoyConfig []byte

//go:embed envoy_ipv4.yaml
var envoyConfigIPv4 []byte

//go:embed fargate_envoy.yaml
var envoyConfigFargate []byte

//go:embed fargate_envoy_ipv4.yaml
var envoyConfigIPv4Fargate []byte

func RemoveContainers(spec *v1.PodTemplateSpec) {
	for i := 0; i < len(spec.Spec.Containers); i++ {
		if sets.New[string](config.ContainerSidecarEnvoyProxy, config.ContainerSidecarVPN).Has(spec.Spec.Containers[i].Name) {
			spec.Spec.Containers = append(spec.Spec.Containers[:i], spec.Spec.Containers[i+1:]...)
			i--
		}
	}
}

// AddMeshContainer todo envoy support ipv6
func AddMeshContainer(spec *v1.PodTemplateSpec, ns, nodeID string, ipv6 bool, managerNamespace string, secret *v1.Secret, image string) {
	// remove envoy proxy containers if already exist
	RemoveContainers(spec)

	spec.Spec.Containers = append(spec.Spec.Containers, v1.Container{
		Name:    config.ContainerSidecarVPN,
		Image:   image,
		Command: []string{"/bin/sh", "-c"},
		Args: []string{`
echo 1 > /proc/sys/net/ipv4/ip_forward
echo 0 > /proc/sys/net/ipv6/conf/all/disable_ipv6
echo 1 > /proc/sys/net/ipv6/conf/all/forwarding
update-alternatives --set iptables /usr/sbin/iptables-legacy
iptables -P INPUT ACCEPT
ip6tables -P INPUT ACCEPT
iptables -P FORWARD ACCEPT
ip6tables -P FORWARD ACCEPT
iptables -t nat -A PREROUTING ! -p icmp ! -s 127.0.0.1 ! -d ${CIDR4} -j DNAT --to :15006
ip6tables -t nat -A PREROUTING ! -p icmp ! -s 0:0:0:0:0:0:0:1 ! -d ${CIDR6} -j DNAT --to :15006
iptables -t nat -A POSTROUTING ! -p icmp ! -s 127.0.0.1 ! -d ${CIDR4} -j MASQUERADE
ip6tables -t nat -A POSTROUTING ! -p icmp ! -s 0:0:0:0:0:0:0:1 ! -d ${CIDR6} -j MASQUERADE
kubevpn server -l "tun:/tcp://${TrafficManagerService}:10801?net=${TunIPv4}&net6=${TunIPv6}&route=${CIDR4}"`,
		},
		Env: []v1.EnvVar{
			{
				Name:  "CIDR4",
				Value: config.CIDR.String(),
			},
			{
				Name:  "CIDR6",
				Value: config.CIDR6.String(),
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
				Name:  "TrafficManagerService",
				Value: fmt.Sprintf("%s.%s", config.ConfigMapPodTrafficManager, managerNamespace),
			},
			{
				Name: config.EnvPodNamespace,
				ValueFrom: &v1.EnvVarSource{
					FieldRef: &v1.ObjectFieldSelector{
						FieldPath: "metadata.namespace",
					},
				},
			},
			{
				Name: config.EnvPodName,
				ValueFrom: &v1.EnvVarSource{
					FieldRef: &v1.ObjectFieldSelector{
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
		SecurityContext: &v1.SecurityContext{
			Capabilities: &v1.Capabilities{
				Add: []v1.Capability{
					"NET_ADMIN",
					//"SYS_MODULE",
				},
			},
			RunAsUser:  pointer.Int64(0),
			RunAsGroup: pointer.Int64(0),
			Privileged: pointer.Bool(true),
		},
	})
	spec.Spec.Containers = append(spec.Spec.Containers, v1.Container{
		Name:  config.ContainerSidecarEnvoyProxy,
		Image: image,
		Command: []string{
			"envoy",
			"-l",
			util.If(config.Debug, log.DebugLevel, log.InfoLevel).String(),
			"--base-id",
			"1",
			"--service-node",
			util.GenEnvoyUID(ns, nodeID),
			"--service-cluster",
			util.GenEnvoyUID(ns, nodeID),
			"--config-yaml",
		},
		Args: []string{
			func() string {
				if ipv6 {
					return GetEnvoyConfig(string(envoyConfig), fmt.Sprintf("%s.%s", config.ConfigMapPodTrafficManager, managerNamespace))
				}
				return GetEnvoyConfig(string(envoyConfigIPv4), fmt.Sprintf("%s.%s", config.ConfigMapPodTrafficManager, managerNamespace))
			}(),
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

func AddEnvoyContainer(spec *v1.PodTemplateSpec, ns, nodeID string, ipv6 bool, managerNamespace string, image string) {
	// remove envoy proxy containers if already exist
	RemoveContainers(spec)

	spec.Spec.Containers = append(spec.Spec.Containers, v1.Container{
		Name:    config.ContainerSidecarVPN,
		Image:   image,
		Command: []string{"kubevpn"},
		Args: []string{
			"server",
			"-l ssh://:2222",
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
		SecurityContext: &v1.SecurityContext{},
	})
	spec.Spec.Containers = append(spec.Spec.Containers, v1.Container{
		Name:  config.ContainerSidecarEnvoyProxy,
		Image: image,
		Command: []string{
			"envoy",
			"-l",
			util.If(config.Debug, log.DebugLevel, log.InfoLevel).String(),
			"--base-id",
			"1",
			"--service-node",
			util.GenEnvoyUID(ns, nodeID),
			"--service-cluster",
			util.GenEnvoyUID(ns, nodeID),
			"--config-yaml",
		},
		Args: []string{
			func() string {
				if ipv6 {
					return GetEnvoyConfig(string(envoyConfigFargate), fmt.Sprintf("%s.%s", config.ConfigMapPodTrafficManager, managerNamespace))
				}
				return GetEnvoyConfig(string(envoyConfigIPv4Fargate), fmt.Sprintf("%s.%s", config.ConfigMapPodTrafficManager, managerNamespace))
			}(),
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

func GetEnvoyConfig(tmplStr string, value string) string {
	tmpl, err := template.New("").Parse(tmplStr)
	if err != nil {
		return ""
	}
	buf := new(bytes.Buffer)
	err = tmpl.Execute(buf, value)
	if err != nil {
		return ""
	}
	return buf.String()
}
