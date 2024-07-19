package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	log "github.com/sirupsen/logrus"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/cert"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/util/podutils"
	"k8s.io/utils/pointer"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func createOutboundPod(ctx context.Context, factory cmdutil.Factory, clientset *kubernetes.Clientset, namespace string) (err error) {
	innerIpv4CIDR := net.IPNet{IP: config.RouterIP, Mask: config.CIDR.Mask}
	innerIpv6CIDR := net.IPNet{IP: config.RouterIP6, Mask: config.CIDR6.Mask}

	service, err := clientset.CoreV1().Services(namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err == nil {
		var pod *v1.Pod
		pod, err = polymorphichelpers.AttachablePodForObjectFn(factory, service, 2*time.Second)
		if err == nil && pod.DeletionTimestamp.IsZero() && podutils.IsPodReady(pod) {
			log.Infoln("traffic manager already exist, reuse it")
			return
		}
	}
	var deleteResource = func(ctx context.Context) {
		options := metav1.DeleteOptions{}
		_ = clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Delete(ctx, config.ConfigMapPodTrafficManager+"."+namespace, options)
		_ = clientset.RbacV1().RoleBindings(namespace).Delete(ctx, config.ConfigMapPodTrafficManager, options)
		_ = clientset.RbacV1().Roles(namespace).Delete(ctx, config.ConfigMapPodTrafficManager, options)
		_ = clientset.CoreV1().ServiceAccounts(namespace).Delete(ctx, config.ConfigMapPodTrafficManager, options)
		_ = clientset.CoreV1().Services(namespace).Delete(ctx, config.ConfigMapPodTrafficManager, options)
		_ = clientset.AppsV1().Deployments(namespace).Delete(ctx, config.ConfigMapPodTrafficManager, options)
	}
	defer func() {
		if err != nil {
			deleteResource(context.Background())
		}
	}()
	deleteResource(ctx)
	log.Infoln("traffic manager not exist, try to create it...")

	// 1) label namespace
	log.Infof("label namespace %s", namespace)
	ns, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		log.Errorf("get namespace error: %s", err.Error())
		return err
	}
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	ns.Labels["ns"] = namespace
	_, err = clientset.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{})
	if err != nil {
		log.Infof("label namespace error: %s", err.Error())
		return err
	}

	// 2) create serviceAccount
	log.Infof("create serviceAccount %s", config.ConfigMapPodTrafficManager)
	_, err = clientset.CoreV1().ServiceAccounts(namespace).Create(ctx, &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: namespace,
		},
		AutomountServiceAccountToken: pointer.Bool(true),
	}, metav1.CreateOptions{})
	if err != nil {
		log.Infof("create serviceAccount error: %s", err.Error())
		return err
	}

	// 3) create roles
	log.Infof("create roles %s", config.ConfigMapPodTrafficManager)
	_, err = clientset.RbacV1().Roles(namespace).Create(ctx, &rbacv1.Role{
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
	}, metav1.CreateOptions{})
	if err != nil {
		log.Errorf("create roles error: %s", err.Error())
		return err
	}

	// 4) create roleBinding
	log.Infof("create roleBinding %s", config.ConfigMapPodTrafficManager)
	_, err = clientset.RbacV1().RoleBindings(namespace).Create(ctx, &rbacv1.RoleBinding{
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
	}, metav1.CreateOptions{})
	if err != nil {
		log.Errorf("create roleBinding error: %s", err.Error())
		return err
	}

	// 5) create service
	log.Infof("create service %s", config.ConfigMapPodTrafficManager)
	udp8422 := "8422-for-udp"
	tcp10800 := "10800-for-tcp"
	tcp9002 := "9002-for-envoy"
	tcp80 := "80-for-webhook"
	udp53 := "53-for-dns"
	_, err = clientset.CoreV1().Services(namespace).Create(ctx, &v1.Service{
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
	}, metav1.CreateOptions{})
	if err != nil {
		log.Errorf("create service error: %s", err.Error())
		return err
	}

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

	domain := util.GetTlsDomain(namespace)
	var crt, key []byte
	crt, key, err = cert.GenerateSelfSignedCertKey(domain, nil, nil)
	if err != nil {
		log.Errorf("generate self signed cert and key error: %s", err.Error())
		return err
	}
	// reason why not use v1.SecretTypeTls is because it needs key called tls.crt and tls.key, but tls.key can not as env variable
	// ➜  ~ export tls.key=a
	//export: not valid in this context: tls.key
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			config.TLSCertKey:       crt,
			config.TLSPrivateKeyKey: key,
		},
		Type: v1.SecretTypeOpaque,
	}
	_, err = clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		log.Errorf("create secret error: %s", err.Error())
		return err
	}

	// 6) create deployment
	log.Infof("create deployment %s", config.ConfigMapPodTrafficManager)
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
							Args: []string{`
sysctl -w net.ipv4.ip_forward=1
sysctl -w net.ipv6.conf.all.disable_ipv6=0
sysctl -w net.ipv6.conf.all.forwarding=1
update-alternatives --set iptables /usr/sbin/iptables-legacy
iptables -F
ip6tables -F
iptables -P INPUT ACCEPT
ip6tables -P INPUT ACCEPT
iptables -P FORWARD ACCEPT
ip6tables -P FORWARD ACCEPT
iptables -t nat -A POSTROUTING -s ${CIDR4} -o eth0 -j MASQUERADE
ip6tables -t nat -A POSTROUTING -s ${CIDR6} -o eth0 -j MASQUERADE
kubevpn serve -L "tcp://:10800" -L "tun://:8422?net=${TunIPv4}" -L "gtcp://:10801" -L "gudp://:10802" --debug=true`,
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
									Add: []v1.Capability{
										"NET_ADMIN",
										//"SYS_MODULE",
									},
								},
								RunAsUser:  pointer.Int64(0),
								RunAsGroup: pointer.Int64(0),
								Privileged: pointer.Bool(true),
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
							Name:    "webhook",
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
							Env:             []v1.EnvVar{},
							ImagePullPolicy: v1.PullIfNotPresent,
							Resources:       resourcesSmall,
						},
					},
					RestartPolicy: v1.RestartPolicyAlways,
				},
			},
		},
	}
	deploy, err = clientset.AppsV1().Deployments(namespace).Create(ctx, deploy, metav1.CreateOptions{})
	if err != nil {
		log.Errorf("Failed to create deployment for %s: %v", config.ConfigMapPodTrafficManager, err)
		return err
	}
	str := fields.OneTermEqualSelector("app", config.ConfigMapPodTrafficManager).String()
	_, selector, err := polymorphichelpers.SelectorsForObject(deploy)
	if err == nil {
		str = selector.String()
	}
	watchStream, err := clientset.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: str,
	})
	if err != nil {
		log.Errorf("Failed to create watch for %s: %v", config.ConfigMapPodTrafficManager, err)
		return err
	}
	defer watchStream.Stop()
	var ok bool
	ctx2, cancelFunc := context.WithTimeout(ctx, time.Minute*60)
	defer cancelFunc()
	wait.UntilWithContext(ctx2, func(ctx context.Context) {
		podList, err := clientset.CoreV1().Pods(namespace).List(ctx2, metav1.ListOptions{
			LabelSelector: fields.OneTermEqualSelector("app", config.ConfigMapPodTrafficManager).String(),
		})
		if err != nil {
			log.Errorf("Failed to list pods for %s: %v", config.ConfigMapPodTrafficManager, err)
			return
		}

		for _, podT := range podList.Items {
			podT := &podT
			if podT.DeletionTimestamp != nil {
				continue
			}
			var sb = bytes.NewBuffer(nil)
			sb.WriteString(fmt.Sprintf("pod %s is %s\n", podT.Name, podT.Status.Phase))
			if podT.Status.Reason != "" {
				sb.WriteString(fmt.Sprintf(" reason %s", podT.Status.Reason))
			}
			if podT.Status.Message != "" {
				sb.WriteString(fmt.Sprintf(" message %s", podT.Status.Message))
			}
			util.PrintStatus(podT, sb)
			log.Infof(sb.String())

			if podutils.IsPodReady(podT) && func() bool {
				for _, status := range podT.Status.ContainerStatuses {
					if !status.Ready {
						return false
					}
				}
				return true
			}() {
				cancelFunc()
				ok = true
			}
		}
	}, time.Second*3)
	if !ok {
		log.Errorf("wait pod %s to be ready timeout", config.ConfigMapPodTrafficManager)
		return errors.New(fmt.Sprintf("wait pod %s to be ready timeout", config.ConfigMapPodTrafficManager))
	}

	// 7) create mutatingWebhookConfigurations
	log.Infof("Creating mutatingWebhook_configuration for %s", config.ConfigMapPodTrafficManager)
	_, err = clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Create(ctx, &admissionv1.MutatingWebhookConfiguration{
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
	}, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsForbidden(err) && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create MutatingWebhookConfigurations, err: %v", err)
	}

	return nil
}
