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

// routeRBACName is the name of the Role/RoleBinding that grants the traffic manager's
// ServiceAccount permission to list/watch pods and services in a workload namespace,
// for server-side route/service discovery (WatchNamespaceRoutes). It is distinct from
// the manager's own Role so it never clobbers it when workload ns == manager ns.
var routeRBACName = config.ConfigMapPodTrafficManager + "-route"

// genRouteRole grants list/watch on pods and services in the given namespace. list/watch
// cannot be restricted by resourceNames, so this is a namespaced (not cluster) grant with
// no resource-name filter — the least-privilege way to let the manager discover routes in
// one namespace without any cluster-scoped RBAC object.
func genRouteRole(namespace string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeRBACName,
			Namespace: namespace,
		},
		Rules: []rbacv1.PolicyRule{{
			Verbs:     []string{"list", "watch"},
			APIGroups: []string{""},
			Resources: []string{"pods", "services"},
		}},
	}
}

// genRouteRoleBinding binds genRouteRole (in roleNamespace, a workload namespace) to the
// traffic manager ServiceAccount living in saNamespace (the manager namespace). In the
// non-central case the two namespaces are the same.
func genRouteRoleBinding(roleNamespace, saNamespace string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeRBACName,
			Namespace: roleNamespace,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: saNamespace,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     routeRBACName,
		},
	}
}

// proxyRBACName is the name of the Role/RoleBinding that grants the traffic manager's
// ServiceAccount permission to inject sidecars into workloads in a workload namespace
// (server-side ProxyInject). Distinct from the manager's own Role and the -route Role
// so the three never clobber each other when workload ns == manager ns.
var proxyRBACName = config.ConfigMapPodTrafficManager + "-proxy"

// genProxyRole grants the verbs needed to inject/uninject sidecars in the given
// namespace: patch/update workload controllers and their pods (rollout wait needs
// list/watch on pods and replicasets), and get/update services (fargate target-port
// remap). Namespaced only — no cluster-scoped RBAC. list/watch cannot be filtered by
// resourceNames, so the rules are namespaced without a name filter.
func genProxyRole(namespace string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      proxyRBACName,
			Namespace: namespace,
		},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:     []string{"get", "list", "watch", "patch", "update"},
				APIGroups: []string{"apps"},
				Resources: []string{"deployments", "statefulsets", "daemonsets", "replicasets"},
			},
			{
				Verbs:     []string{"get", "list", "watch", "create", "delete", "patch", "update"},
				APIGroups: []string{""},
				Resources: []string{"pods"},
			},
			{
				Verbs:     []string{"get", "list", "watch", "patch", "update"},
				APIGroups: []string{""},
				Resources: []string{"replicationcontrollers", "services"},
			},
		},
	}
}

// genProxyRoleBinding binds genProxyRole (in roleNamespace, a workload namespace) to
// the traffic manager ServiceAccount in saNamespace (the manager namespace). In the
// non-central case the two namespaces are the same.
func genProxyRoleBinding(roleNamespace, saNamespace string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      proxyRBACName,
			Namespace: roleNamespace,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: saNamespace,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     proxyRBACName,
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
				Port:       config.PortXDS,
				TargetPort: intstr.FromInt32(config.PortXDS),
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
			// Recreate (not the default RollingUpdate) guarantees at most one
			// traffic-manager pod at a time. The xds TunConfigServer
			// keeps its IP-lease state in an in-memory map serialized by a single
			// mutex; two concurrent pods during a rollout would diverge and the
			// blind ConfigMap writes would clobber each other, losing leases of
			// connected clients. Clients keep their IPs across the brief restart
			// (state is persisted in the ConfigMap), and the data plane (client
			// TUN/routes, workload envoy sidecars) is unaffected.
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RecreateDeploymentStrategyType,
			},
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
								"--debug",
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
							Name:    config.ContainerSidecarXDS,
							Image:   image,
							Command: []string{"kubevpn"},
							Args:    []string{"xds", "--debug"},
							Ports: []v1.ContainerPort{{
								Name:          tcp9002,
								ContainerPort: config.PortXDS,
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
							Args:    []string{"dns", "--debug"},
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
	containers[0].LivenessProbe, containers[0].ReadinessProbe, containers[0].StartupProbe = tcpProbes(config.PortUDP)
	containers[1].LivenessProbe, containers[1].ReadinessProbe, containers[1].StartupProbe = tcpProbes(config.PortXDS)

	if imagePullSecretName != "" {
		deploy.Spec.Template.Spec.ImagePullSecrets = []v1.LocalObjectReference{{
			Name: imagePullSecretName,
		}}
	}
	return deploy
}
