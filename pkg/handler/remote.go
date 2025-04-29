package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/util/podutils"
	"k8s.io/utils/pointer"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func createOutboundPod(ctx context.Context, clientset *kubernetes.Clientset, namespace string, gvisor bool, imagePullSecretName string) (err error) {
	var exists bool
	exists, err = util.DetectPodExists(ctx, clientset, namespace)
	if err != nil {
		return err
	}
	if exists {
		plog.G(ctx).Infof("Use exist traffic manager in namespace %s", namespace)
		return nil
	}

	var deleteResource = func(ctx context.Context) {
		options := metav1.DeleteOptions{GracePeriodSeconds: pointer.Int64(0)}
		name := config.ConfigMapPodTrafficManager
		_ = clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Delete(ctx, name+"."+namespace, options)
		_ = clientset.RbacV1().RoleBindings(namespace).Delete(ctx, name, options)
		_ = clientset.RbacV1().Roles(namespace).Delete(ctx, name, options)
		_ = clientset.CoreV1().ServiceAccounts(namespace).Delete(ctx, name, options)
		_ = clientset.CoreV1().Services(namespace).Delete(ctx, name, options)
		_ = clientset.CoreV1().Secrets(namespace).Delete(ctx, name, options)
		_ = clientset.CoreV1().Pods(namespace).Delete(ctx, config.CniNetName, options)
		_ = clientset.AppsV1().Deployments(namespace).Delete(ctx, name, options)
	}
	defer func() {
		if err != nil {
			deleteResource(context.Background())
		}
	}()
	deleteResource(ctx)

	// 1) label namespace
	plog.G(ctx).Infof("Labeling Namespace %s", namespace)
	ns, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Get Namespace error: %s", err.Error())
		return err
	}
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	ns.Labels["ns"] = namespace
	_, err = clientset.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{})
	if err != nil {
		plog.G(ctx).Infof("Labeling Namespace error: %s", err.Error())
		return err
	}

	// 2) create serviceAccount
	plog.G(ctx).Infof("Creating ServiceAccount %s", config.ConfigMapPodTrafficManager)
	_, err = clientset.CoreV1().ServiceAccounts(namespace).Create(ctx, genServiceAccount(namespace), metav1.CreateOptions{})
	if err != nil {
		plog.G(ctx).Infof("Creating ServiceAccount error: %s", err.Error())
		return err
	}

	// 3) create roles
	plog.G(ctx).Infof("Creating Roles %s", config.ConfigMapPodTrafficManager)
	_, err = clientset.RbacV1().Roles(namespace).Create(ctx, genRole(namespace), metav1.CreateOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Creating Roles error: %s", err.Error())
		return err
	}

	// 4) create roleBinding
	plog.G(ctx).Infof("Creating RoleBinding %s", config.ConfigMapPodTrafficManager)
	_, err = clientset.RbacV1().RoleBindings(namespace).Create(ctx, genRoleBinding(namespace), metav1.CreateOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Creating RoleBinding error: %s", err.Error())
		return err
	}

	// 5) create service
	plog.G(ctx).Infof("Creating Service %s", config.ConfigMapPodTrafficManager)
	udp8422 := "8422-for-udp"
	tcp10800 := "10800-for-tcp"
	tcp9002 := "9002-for-envoy"
	tcp80 := "80-for-webhook"
	udp53 := "53-for-dns"
	svcSpec := genService(namespace, udp8422, tcp10800, tcp9002, tcp80, udp53)
	_, err = clientset.CoreV1().Services(namespace).Create(ctx, svcSpec, metav1.CreateOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Creating Service error: %s", err.Error())
		return err
	}

	crt, key, host, err := util.GenTLSCert(ctx, namespace)
	if err != nil {
		return err
	}
	// reason why not use v1.SecretTypeTls is because it needs key called tls.crt and tls.key, but tls.key can not as env variable
	// ➜  ~ export tls.key=a
	//export: not valid in this context: tls.key
	secret := genSecret(namespace, crt, key, host)
	_, err = clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Creating secret error: %s", err.Error())
		return err
	}

	// 6) create mutatingWebhookConfigurations
	plog.G(ctx).Infof("Creating MutatingWebhookConfiguration %s", config.ConfigMapPodTrafficManager)
	mutatingWebhookConfiguration := genMutatingWebhookConfiguration(namespace, crt)
	_, err = clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Create(ctx, mutatingWebhookConfiguration, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create MutatingWebhookConfigurations: %v", err)
	}

	// 7) create deployment
	plog.G(ctx).Infof("Creating Deployment %s", config.ConfigMapPodTrafficManager)
	deploy := genDeploySpec(namespace, udp8422, tcp10800, tcp9002, udp53, tcp80, gvisor, imagePullSecretName)
	deploy, err = clientset.AppsV1().Deployments(namespace).Create(ctx, deploy, metav1.CreateOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Failed to create deployment for %s: %v", config.ConfigMapPodTrafficManager, err)
		return err
	}

	return waitPodReady(ctx, deploy, clientset.CoreV1().Pods(namespace))
}

func genServiceAccount(namespace string) *v1.ServiceAccount {
	return &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: namespace,
		},
		AutomountServiceAccountToken: pointer.Bool(true),
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
					Path:      pointer.String("/pods"),
					Port:      pointer.Int32(80),
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
			// same as above label ns
			NamespaceSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"ns": namespace}},
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

func genService(namespace string, udp8422 string, tcp10800 string, tcp9002 string, tcp80 string, udp53 string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: namespace,
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{{
				Name:       udp8422,
				Protocol:   v1.ProtocolUDP,
				Port:       8422,
				TargetPort: intstr.FromInt32(8422),
			}, {
				Name:       tcp10800,
				Protocol:   v1.ProtocolTCP,
				Port:       10800,
				TargetPort: intstr.FromInt32(10800),
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

func genDeploySpec(namespace string, udp8422 string, tcp10800 string, tcp9002 string, udp53 string, tcp80 string, gvisor bool, imagePullSecretName string) *appsv1.Deployment {
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

	innerIpv4CIDR := net.IPNet{IP: config.RouterIP, Mask: config.CIDR.Mask}
	innerIpv6CIDR := net.IPNet{IP: config.RouterIP6, Mask: config.CIDR6.Mask}

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: pointer.Int32(1),
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
					Volumes: []v1.Volume{{
						Name: config.VolumeEnvoyConfig,
						VolumeSource: v1.VolumeSource{
							ConfigMap: &v1.ConfigMapVolumeSource{
								LocalObjectReference: v1.LocalObjectReference{
									Name: config.ConfigMapPodTrafficManager,
								},
								Items: []v1.KeyToPath{
									{
										Key:  config.KeyEnvoy,
										Path: "envoy-config.yaml",
									},
								},
								Optional: pointer.Bool(false),
							},
						},
					}},
					Containers: []v1.Container{
						{
							Name:    config.ContainerSidecarVPN,
							Image:   config.Image,
							Command: []string{"/bin/sh", "-c"},
							Args: []string{util.If(
								gvisor,
								`
kubevpn server -l "tcp://:10800" -l "gtcp://:10801"`,
								`
echo 1 > /proc/sys/net/ipv4/ip_forward
echo 0 > /proc/sys/net/ipv6/conf/all/disable_ipv6
echo 1 > /proc/sys/net/ipv6/conf/all/forwarding
update-alternatives --set iptables /usr/sbin/iptables-legacy
iptables -P INPUT ACCEPT
ip6tables -P INPUT ACCEPT
iptables -P FORWARD ACCEPT
ip6tables -P FORWARD ACCEPT
iptables -t nat -A POSTROUTING -s ${CIDR4} -o eth0 -j MASQUERADE
ip6tables -t nat -A POSTROUTING -s ${CIDR6} -o eth0 -j MASQUERADE
kubevpn server -l "tcp://:10800" -l "tun://:8422?net=${TunIPv4}&net6=${TunIPv6}" -l "gtcp://:10801"`,
							)},
							EnvFrom: []v1.EnvFromSource{{
								SecretRef: &v1.SecretEnvSource{
									LocalObjectReference: v1.LocalObjectReference{
										Name: config.ConfigMapPodTrafficManager,
									},
								},
							}},
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
									Value: innerIpv4CIDR.String(),
								},
								{
									Name:  config.EnvInboundPodTunIPv6,
									Value: innerIpv6CIDR.String(),
								},
							},
							Ports: []v1.ContainerPort{{
								Name:          udp8422,
								ContainerPort: 8422,
								Protocol:      v1.ProtocolUDP,
							}, {
								Name:          tcp10800,
								ContainerPort: 10800,
								Protocol:      v1.ProtocolTCP,
							}},
							Resources:       resourcesLarge,
							ImagePullPolicy: v1.PullIfNotPresent,
							SecurityContext: &v1.SecurityContext{
								Capabilities: &v1.Capabilities{
									Add: util.If(gvisor,
										[]v1.Capability{},
										[]v1.Capability{
											"NET_ADMIN",
											//"SYS_MODULE",
										},
									),
								},
								RunAsUser:  pointer.Int64(0),
								RunAsGroup: pointer.Int64(0),
								Privileged: util.If(gvisor, pointer.Bool(false), pointer.Bool(true)),
							},
						},
						{
							Name:    config.ContainerSidecarControlPlane,
							Image:   config.Image,
							Command: []string{"kubevpn"},
							Args:    []string{"control-plane", "--watchDirectoryFilename", "/etc/envoy/envoy-config.yaml"},
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
							VolumeMounts: []v1.VolumeMount{
								{
									Name:      config.VolumeEnvoyConfig,
									ReadOnly:  true,
									MountPath: "/etc/envoy",
								},
							},
							ImagePullPolicy: v1.PullIfNotPresent,
							Resources:       resourcesSmall,
						},
						{
							Name:    config.ContainerSidecarWebhook,
							Image:   config.Image,
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
	if imagePullSecretName != "" {
		deploy.Spec.Template.Spec.ImagePullSecrets = []v1.LocalObjectReference{{
			Name: imagePullSecretName,
		}}
	}
	return deploy
}

func waitPodReady(ctx context.Context, deploy *appsv1.Deployment, clientset corev1.PodInterface) error {
	_, selector, err := polymorphichelpers.SelectorsForObject(deploy)
	if err != nil {
		return err
	}
	var isPodReady bool
	var lastMessage string
	ctx2, cancelFunc := context.WithTimeout(ctx, time.Minute*60)
	defer cancelFunc()
	plog.G(ctx).Infoln()
	wait.UntilWithContext(ctx2, func(ctx context.Context) {
		podList, err := clientset.List(ctx2, metav1.ListOptions{
			LabelSelector: selector.String(),
		})
		if err != nil {
			plog.G(ctx).Errorf("Failed to list pods for %s: %v", deploy.Name, err)
			return
		}

		for _, pod := range podList.Items {
			if pod.DeletionTimestamp != nil {
				continue
			}
			var sb = bytes.NewBuffer(nil)
			sb.WriteString(fmt.Sprintf("Pod %s is %s...\n", pod.Name, pod.Status.Phase))
			if pod.Status.Reason != "" {
				sb.WriteString(fmt.Sprintf(" reason %s", pod.Status.Reason))
			}
			if pod.Status.Message != "" {
				sb.WriteString(fmt.Sprintf(" message %s", pod.Status.Message))
			}
			util.PrintStatus(&pod, sb)
			if lastMessage != sb.String() {
				plog.G(ctx).Info(sb.String())
			}
			lastMessage = sb.String()

			readyFunc := func(pod *v1.Pod) bool {
				for _, status := range pod.Status.ContainerStatuses {
					if !status.Ready {
						return false
					}
				}
				return true
			}
			if podutils.IsPodReady(&pod) && readyFunc(&pod) {
				cancelFunc()
				isPodReady = true
			}
		}
	}, time.Second*3)

	if !isPodReady {
		plog.G(ctx).Errorf("Wait pod %s to be ready timeout", deploy.Name)
		return errors.New(fmt.Sprintf("wait pod %s to be ready timeout", deploy.Name))
	}

	return nil
}
