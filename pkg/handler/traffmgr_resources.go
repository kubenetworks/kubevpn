package handler

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func genServiceAccount(namespace string) *v1.ServiceAccount {
	return &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: namespace,
		},
		AutomountServiceAccountToken: ptr.To(true),
	}
}

func genRole(namespace string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: namespace,
		},
		Rules: []rbacv1.PolicyRule{{
			Verbs:         []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			APIGroups:     []string{""},
			Resources:     []string{"configmaps", "secrets"},
			ResourceNames: []string{config.ConfigMapPodTrafficManager},
		}},
	}
}

func genRoleBinding(namespace string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: namespace,
		},
		Subjects: []rbacv1.Subject{{
			Kind: "ServiceAccount",
			//APIGroup:  "rbac.authorization.k8s.io",
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: namespace,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     config.ConfigMapPodTrafficManager,
		},
	}
}

func genService(namespace string) *v1.Service {
	tcp10801 := config.PortNameTCP
	tcp9002 := config.PortNameEnvoy
	udp53 := config.PortNameDNS
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: namespace,
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{{
				Name:       tcp10801,
				Protocol:   v1.ProtocolTCP,
				Port:       config.PortTCP,
				TargetPort: intstr.FromInt32(config.PortTCP),
			}, {
				Name:       tcp9002,
				Protocol:   v1.ProtocolTCP,
				Port:       config.PortControlPlane,
				TargetPort: intstr.FromInt32(config.PortControlPlane),
			}, {
				Name:       udp53,
				Protocol:   v1.ProtocolUDP,
				Port:       config.PortDNS,
				TargetPort: intstr.FromInt32(config.PortDNS),
			}},
			Selector: map[string]string{"app": config.ConfigMapPodTrafficManager},
			Type:     v1.ServiceTypeClusterIP,
		},
	}
}

func genSecret(namespace string, crt []byte, key []byte, host []byte) *v1.Secret {
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			config.TLSCertKey:       crt,
			config.TLSPrivateKeyKey: key,
			config.TLSServerName:    host,
		},
		Type: v1.SecretTypeOpaque,
	}
	return secret
}

func tcpProbes(port int32) (*v1.Probe, *v1.Probe, *v1.Probe) {
	handler := v1.ProbeHandler{TCPSocket: &v1.TCPSocketAction{Port: intstr.FromInt32(port)}}
	return &v1.Probe{ProbeHandler: handler, InitialDelaySeconds: 5, PeriodSeconds: 15, FailureThreshold: 3},
		&v1.Probe{ProbeHandler: handler, InitialDelaySeconds: 3, PeriodSeconds: 10, FailureThreshold: 3},
		&v1.Probe{ProbeHandler: handler, InitialDelaySeconds: 1, PeriodSeconds: 2, FailureThreshold: 15}
}

func genDeploySpec(namespace, image, imagePullSecretName string) *appsv1.Deployment {
	tcp10801 := config.PortNameTCP
	tcp9002 := config.PortNameEnvoy
	udp53 := config.PortNameDNS
	var resourcesSmall = v1.ResourceRequirements{
		Requests: map[v1.ResourceName]resource.Quantity{
			v1.ResourceCPU:    resource.MustParse("100m"),
			v1.ResourceMemory: resource.MustParse("128Mi"),
		},
		Limits: map[v1.ResourceName]resource.Quantity{
			v1.ResourceCPU:    resource.MustParse("200m"),
			v1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	var resourcesLarge = v1.ResourceRequirements{
		Requests: map[v1.ResourceName]resource.Quantity{
			v1.ResourceCPU:    resource.MustParse("500m"),
			v1.ResourceMemory: resource.MustParse("512Mi"),
		},
		Limits: map[v1.ResourceName]resource.Quantity{
			v1.ResourceCPU:    resource.MustParse("2000m"),
			v1.ResourceMemory: resource.MustParse("2048Mi"),
		},
	}

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": config.ConfigMapPodTrafficManager},
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": config.ConfigMapPodTrafficManager},
					Annotations: map[string]string{
						"sidecar.istio.io/inject": "false",
					},
				},
				Spec: v1.PodSpec{
					ServiceAccountName: config.ConfigMapPodTrafficManager,
					Volumes:            []v1.Volume{},
					Containers: []v1.Container{
						{
							Name:    config.ContainerSidecarVPN,
							Image:   image,
							Command: []string{"kubevpn"},
							Args: []string{
								"server",
								fmt.Sprintf("-l gtcp://:%d", config.PortTCP),
								fmt.Sprintf("-l gudp://:%d", config.PortUDP),
							},
							EnvFrom: []v1.EnvFromSource{{
								SecretRef: &v1.SecretEnvSource{
									LocalObjectReference: v1.LocalObjectReference{
										Name: config.ConfigMapPodTrafficManager,
									},
								},
							}},
							Env: []v1.EnvVar{},
							Ports: []v1.ContainerPort{{
								Name:          tcp10801,
								ContainerPort: config.PortTCP,
								Protocol:      v1.ProtocolTCP,
							}},
							Resources:       resourcesLarge,
							ImagePullPolicy: v1.PullIfNotPresent,
							SecurityContext: &v1.SecurityContext{
								Capabilities: &v1.Capabilities{
									Add: []v1.Capability{},
								},
								Privileged: ptr.To(false),
							},
						},
						{
							Name:    config.ContainerSidecarControlPlane,
							Image:   image,
							Command: []string{"kubevpn"},
							Args:    []string{"control-plane"},
							Ports: []v1.ContainerPort{{
								Name:          tcp9002,
								ContainerPort: config.PortControlPlane,
								Protocol:      v1.ProtocolTCP,
							}},
							VolumeMounts:    []v1.VolumeMount{},
							ImagePullPolicy: v1.PullIfNotPresent,
							Resources:       resourcesSmall,
						},
						{
							Name:    config.ContainerSidecarDNS,
							Image:   image,
							Command: []string{"kubevpn"},
							Args:    []string{"dns"},
							Ports: []v1.ContainerPort{{
								Name:          udp53,
								ContainerPort: 53,
								Protocol:      v1.ProtocolUDP,
							}},
							ImagePullPolicy: v1.PullIfNotPresent,
							Resources:       resourcesSmall,
						},
					},
					RestartPolicy: v1.RestartPolicyAlways,
				},
			},
		},
	}
	containers := deploy.Spec.Template.Spec.Containers
	containers[0].LivenessProbe, containers[0].ReadinessProbe, containers[0].StartupProbe = tcpProbes(config.PortTCP)
	containers[1].LivenessProbe, containers[1].ReadinessProbe, containers[1].StartupProbe = tcpProbes(config.PortControlPlane)

	if imagePullSecretName != "" {
		deploy.Spec.Template.Spec.ImagePullSecrets = []v1.LocalObjectReference{{
			Name: imagePullSecretName,
		}}
	}
	return deploy
}
