package mesh

import (
	"github.com/wencaiwulue/kubevpn/util"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"
)

const (
	VPN          = "vpn"
	EnvoyProxy   = "envoy-proxy"
	ControlPlane = "control-plane"
	EnvoyConfig  = "envoy-config"
)

func AddMeshContainer(spec *v1.PodTemplateSpec, configMapName string, c util.PodRouteConfig) {
	zero := int64(0)
	t := true
	spec.Spec.Volumes = append(spec.Spec.Volumes, v1.Volume{
		Name: EnvoyConfig,
		VolumeSource: v1.VolumeSource{
			ConfigMap: &v1.ConfigMapVolumeSource{
				LocalObjectReference: v1.LocalObjectReference{
					Name: configMapName,
				},
				Items: []v1.KeyToPath{
					{
						Key:  "base-envoy.yaml",
						Path: "base-envoy.yaml",
					},
					{
						Key:  "envoy-config.yaml",
						Path: "envoy-config.yaml",
					},
				},
			},
		},
	})
	spec.Spec.Containers = append(spec.Spec.Containers, v1.Container{
		Name:    VPN,
		Image:   "naison/kubevpn:v2",
		Command: []string{"/bin/sh", "-c"},
		Args: []string{
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
	spec.Spec.Containers = append(spec.Spec.Containers, v1.Container{
		Name:    EnvoyProxy,
		Image:   "naison/kubevpnmesh:v2",
		Command: []string{"/bin/sh", "-c"},
		Args: []string{
			"sysctl net.ipv4.ip_forward=1;" +
				"iptables -F;" +
				"iptables -P INPUT ACCEPT;" +
				"iptables -P FORWARD ACCEPT;" +
				"iptables -t nat -A PREROUTING -i eth0 -p tcp --dport 80:60000 ! -s 127.0.0.1 ! -d 223.254.254.1/24 -j DNAT --to 127.0.0.1:15006;" +
				"iptables -t nat -A POSTROUTING -p tcp -m tcp --dport 80:60000 ! -s 127.0.0.1 ! -d 223.254.254.1/24 -j MASQUERADE;" +
				"iptables -t nat -A PREROUTING -i eth0 -p udp --dport 80:60000 ! -s 127.0.0.1 ! -d 223.254.254.1/24 -j DNAT --to 127.0.0.1:15006;" +
				"iptables -t nat -A POSTROUTING -p udp -m udp --dport 80:60000 ! -s 127.0.0.1 ! -d 223.254.254.1/24 -j MASQUERADE;" +
				"envoy -c /etc/envoy/base-envoy.yaml",
		},
		SecurityContext: &v1.SecurityContext{
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
		VolumeMounts: []v1.VolumeMount{
			{
				Name:      EnvoyConfig,
				ReadOnly:  false,
				MountPath: "/etc/envoy/",
				//SubPath:   "envoy.yaml",
			},
		},
	})
	spec.Spec.Containers = append(spec.Spec.Containers, v1.Container{
		Name:    ControlPlane,
		Image:   "naison/envoy-xds-server:latest",
		Command: []string{"envoy-xds-server"},
		Args:    []string{"--watchDirectoryFileName", "/etc/envoy-config/envoy-config.yaml"},
		VolumeMounts: []v1.VolumeMount{
			{
				Name:      "envoy-config",
				ReadOnly:  false,
				MountPath: "/etc/envoy-config/",
				//SubPath:   "envoy.yaml",
			},
		},
		ImagePullPolicy: v1.PullAlways,
	})
}

func RemoveContainers(spec *v1.PodTemplateSpec) {
	var volumeC []v1.Volume
	for _, volume := range spec.Spec.Volumes {
		if volume.Name != EnvoyConfig {
			volumeC = append(volumeC, volume)
		}
	}
	spec.Spec.Volumes = volumeC
	var containerC []v1.Container
	for _, container := range spec.Spec.Containers {
		if !sets.NewString(EnvoyProxy, ControlPlane, VPN).Has(container.Name) {
			containerC = append(containerC, container)
		}
	}
	spec.Spec.Containers = containerC
}
