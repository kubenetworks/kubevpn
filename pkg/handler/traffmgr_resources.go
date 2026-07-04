package handler

import (
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
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

func genMutatingWebhookConfiguration(namespace string, crt []byte) *admissionv1.MutatingWebhookConfiguration {
	return &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager + "." + namespace,
			Namespace: namespace,
		},
		Webhooks: []admissionv1.MutatingWebhook{{
			Name: config.ConfigMapPodTrafficManager + ".naison.io", // no sense
			ClientConfig: admissionv1.WebhookClientConfig{
				Service: &admissionv1.ServiceReference{
					Namespace: namespace,
					Name:      config.ConfigMapPodTrafficManager,
					Path:      ptr.To("/pods"),
					Port:      ptr.To[int32](80),
				},
				CABundle: crt,
			},
			Rules: []admissionv1.RuleWithOperations{{
				Operations: []admissionv1.OperationType{admissionv1.Create, admissionv1.Delete},
				Rule: admissionv1.Rule{
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"pods"},
					Scope:       ptr.To(admissionv1.NamespacedScope),
				},
			}},
			FailurePolicy: ptr.To(admissionv1.Ignore),
			// namespace kubevpn is special, if installed to this namespace, means center install mode
			// same as above label ns
			NamespaceSelector:       util.If(namespace == config.DefaultNamespaceKubevpn, nil, &metav1.LabelSelector{MatchLabels: map[string]string{"ns": namespace}}),
			SideEffects:             ptr.To(admissionv1.SideEffectClassNone),
			TimeoutSeconds:          ptr.To[int32](15),
			AdmissionReviewVersions: []string{"v1", "v1beta1"},
			ReinvocationPolicy:      ptr.To(admissionv1.NeverReinvocationPolicy),
			/*// needs to enable featureGate=AdmissionWebhookMatchConditions
			MatchConditions: []admissionv1.MatchCondition{
				{
					Name: "",
					Expression: fmt.Sprintf(
						"container_name.exists(c, c == '%s') && environment_variable.find(e, e == '%s').exists()",
						config.ContainerSidecarVPN, config.EnvInboundPodTunIPv4,
					),
				},
			},*/
		}},
	}
}

func genService(namespace string, tcp10801 string, tcp9002 string, tcp80 string, udp53 string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: namespace,
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{{
				Name:       tcp10801,
				Protocol:   v1.ProtocolTCP,
				Port:       10801,
				TargetPort: intstr.FromInt32(10801),
			}, {
				Name:       tcp9002,
				Protocol:   v1.ProtocolTCP,
				Port:       9002,
				TargetPort: intstr.FromInt32(9002),
			}, {
				Name:       tcp80,
				Protocol:   v1.ProtocolTCP,
				Port:       80,
				TargetPort: intstr.FromInt32(80),
			}, {
				Name:       udp53,
				Protocol:   v1.ProtocolUDP,
				Port:       53,
				TargetPort: intstr.FromInt32(53),
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

func httpProbes(port int32, path string) (*v1.Probe, *v1.Probe, *v1.Probe) {
	handler := v1.ProbeHandler{HTTPGet: &v1.HTTPGetAction{Path: path, Port: intstr.FromInt32(port), Scheme: v1.URISchemeHTTPS}}
	return &v1.Probe{ProbeHandler: handler, InitialDelaySeconds: 5, PeriodSeconds: 15, FailureThreshold: 3},
		&v1.Probe{ProbeHandler: handler, InitialDelaySeconds: 3, PeriodSeconds: 10, FailureThreshold: 3},
		&v1.Probe{ProbeHandler: handler, InitialDelaySeconds: 1, PeriodSeconds: 2, FailureThreshold: 15}
}

func genDeploySpec(namespace, tcp10801, tcp9002, udp53, tcp80, image, imagePullSecretName string) *appsv1.Deployment {
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
								"-l gtcp://:10801",
								"-l gudp://:10802",
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
								ContainerPort: 10801,
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
							Ports: []v1.ContainerPort{
								{
									Name:          tcp9002,
									ContainerPort: 9002,
									Protocol:      v1.ProtocolTCP,
								},
								{
									Name:          udp53,
									ContainerPort: 53,
									Protocol:      v1.ProtocolUDP,
								},
							},
							VolumeMounts:    []v1.VolumeMount{},
							ImagePullPolicy: v1.PullIfNotPresent,
							Resources:       resourcesSmall,
						},
						{
							Name:    config.ContainerSidecarWebhook,
							Image:   image,
							Command: []string{"kubevpn"},
							Args:    []string{"webhook"},
							Ports: []v1.ContainerPort{{
								Name:          tcp80,
								ContainerPort: 80,
								Protocol:      v1.ProtocolTCP,
							}},
							EnvFrom: []v1.EnvFromSource{{
								SecretRef: &v1.SecretEnvSource{
									LocalObjectReference: v1.LocalObjectReference{
										Name: config.ConfigMapPodTrafficManager,
									},
								},
							}},
							Env: []v1.EnvVar{{
								Name: config.EnvPodNamespace,
								ValueFrom: &v1.EnvVarSource{
									FieldRef: &v1.ObjectFieldSelector{
										FieldPath: "metadata.namespace",
									},
								},
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
	containers[0].LivenessProbe, containers[0].ReadinessProbe, containers[0].StartupProbe = tcpProbes(10801)
	containers[1].LivenessProbe, containers[1].ReadinessProbe, containers[1].StartupProbe = tcpProbes(9002)
	containers[2].LivenessProbe, containers[2].ReadinessProbe, containers[2].StartupProbe = httpProbes(80, "/readyz")

	if imagePullSecretName != "" {
		deploy.Spec.Template.Spec.ImagePullSecrets = []v1.LocalObjectReference{{
			Name: imagePullSecretName,
		}}
	}
	return deploy
}

