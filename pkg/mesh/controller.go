package mesh

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/pointer"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func RemoveContainers(spec *v1.PodTemplateSpec) {
	for i := 0; i < len(spec.Spec.Containers); i++ {
		if sets.NewString(config.ContainerSidecarEnvoyProxy, config.ContainerSidecarVPN).Has(spec.Spec.Containers[i].Name) {
			spec.Spec.Containers = append(spec.Spec.Containers[:i], spec.Spec.Containers[i+1:]...)
			i--
		}
	}
}

func AddMeshContainer(spec *v1.PodTemplateSpec, nodeId string, c util.PodRouteConfig) {
	// remove envoy proxy containers if already exist
	RemoveContainers(spec)
	spec.Spec.Containers = append(spec.Spec.Containers, v1.Container{
		Name:    config.ContainerSidecarVPN,
		Image:   config.Image,
		Command: []string{"/bin/sh", "-c"},
		Args: []string{`
sysctl net.ipv4.ip_forward=1
update-alternatives --set iptables /usr/sbin/iptables-legacy
iptables -F
iptables -P INPUT ACCEPT
iptables -P FORWARD ACCEPT
iptables -t nat -A PREROUTING ! -p icmp ! -s 127.0.0.1 ! -d ${CIDR} -j DNAT --to 127.0.0.1:15006
iptables -t nat -A POSTROUTING ! -p icmp ! -s 127.0.0.1 ! -d ${CIDR} -j MASQUERADE
kubevpn serve -L "tun:/127.0.0.1:8422?net=${InboundPodTunIP}&route=${CIDR}" -F "tcp://${TrafficManagerRealIP}:10800" --debug=true`,
		},
		EnvFrom: []v1.EnvFromSource{{
			SecretRef: &v1.SecretEnvSource{
				LocalObjectReference: v1.LocalObjectReference{
					Name: config.ConfigMapPodTrafficManager,
				},
			},
		}},
		Env: []v1.EnvVar{
			{
				Name:  "CIDR",
				Value: config.CIDR.String(),
			},
			{
				Name:  "TrafficManagerRealIP",
				Value: c.TrafficManagerRealIP,
			},
			{
				Name:  config.EnvInboundPodTunIP,
				Value: c.InboundPodTunIP,
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
			Privileged: pointer.Bool(true),
		},
	})
	spec.Spec.Containers = append(spec.Spec.Containers, v1.Container{
		Name:    config.ContainerSidecarEnvoyProxy,
		Image:   config.Image,
		Command: []string{"envoy", "-l", "debug", "--base-id", "1", "--config-yaml"},
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
