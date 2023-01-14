package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	pkgresource "k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/retry"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/util/podutils"
	"k8s.io/utils/pointer"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/exchange"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func CreateOutboundPod(ctx context.Context, factory cmdutil.Factory, clientset *kubernetes.Clientset, namespace string, trafficManagerIP string, nodeCIDR []*net.IPNet) (ip net.IP, err error) {
	service, err := clientset.CoreV1().Services(namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err == nil {
		_, err = polymorphichelpers.AttachablePodForObjectFn(factory, service, 2*time.Second)
		if err == nil {
			_, err = updateRefCount(clientset.CoreV1().ConfigMaps(namespace), config.ConfigMapPodTrafficManager, 1)
			if err != nil {
				return
			}
			log.Infoln("traffic manager already exist, reuse it")
			return net.ParseIP(service.Spec.ClusterIP), nil
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
	deleteResource(context.Background())
	log.Infoln("traffic manager not exist, try to create it...")

	// 1) label namespace
	ns, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	ns.Labels["ns"] = namespace
	_, err = clientset.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{})
	if err != nil {
		return nil, err
	}

	// 2) create serviceAccount
	_, err = clientset.CoreV1().ServiceAccounts(namespace).Create(ctx, &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: namespace,
		},
		AutomountServiceAccountToken: pointer.Bool(true),
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	// 3) create roles
	_, err = clientset.RbacV1().Roles(namespace).Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: namespace,
		},
		Rules: []rbacv1.PolicyRule{{
			Verbs:         []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			APIGroups:     []string{""},
			Resources:     []string{"configmaps"},
			ResourceNames: []string{config.ConfigMapPodTrafficManager},
		}},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	// 4) create roleBinding
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
		return nil, err
	}

	udp8422 := "8422-for-udp"
	tcp10800 := "10800-for-tcp"
	tcp9002 := "9002-for-envoy"
	tcp80 := "80-for-webhook"
	svc, err := clientset.CoreV1().Services(namespace).Create(ctx, &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: namespace,
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{{
				Name:       udp8422,
				Protocol:   v1.ProtocolUDP,
				Port:       8422,
				TargetPort: intstr.FromInt(8422),
			}, {
				Name:       tcp10800,
				Protocol:   v1.ProtocolTCP,
				Port:       10800,
				TargetPort: intstr.FromInt(10800),
			}, {
				Name:       tcp9002,
				Protocol:   v1.ProtocolTCP,
				Port:       9002,
				TargetPort: intstr.FromInt(9002),
			}, {
				Name:       tcp80,
				Protocol:   v1.ProtocolTCP,
				Port:       80,
				TargetPort: intstr.FromInt(80),
			}},
			Selector: map[string]string{"app": config.ConfigMapPodTrafficManager},
			Type:     v1.ServiceTypeClusterIP,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	var s = []string{config.CIDR.String()}
	for _, ipNet := range nodeCIDR {
		s = append(s, ipNet.String())
	}

	var Resources = v1.ResourceRequirements{
		Requests: map[v1.ResourceName]resource.Quantity{
			v1.ResourceCPU:    resource.MustParse("128m"),
			v1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: map[v1.ResourceName]resource.Quantity{
			v1.ResourceCPU:    resource.MustParse("256m"),
			v1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}

	h := config.ConfigMapPodTrafficManager + "." + namespace + "." + "svc"
	cert, key, _ := cert.GenerateSelfSignedCertKey(h, nil, nil)

	deployment := &appsv1.Deployment{
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
sysctl net.ipv4.ip_forward=1
update-alternatives --set iptables /usr/sbin/iptables-legacy
iptables -F
iptables -P INPUT ACCEPT
iptables -P FORWARD ACCEPT
iptables -t nat -A POSTROUTING -s ${CIDR} -o eth0 -j MASQUERADE
kubevpn serve -L "tcp://:10800" -L "tun://:8422?net=${TrafficManagerIP}" --debug=true`,
							},
							Env: []v1.EnvVar{
								{
									Name:  "CIDR",
									Value: strings.Join(s, ","),
								},
								{
									Name:  "TrafficManagerIP",
									Value: trafficManagerIP,
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
							Resources:       Resources,
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
						},
						{
							Name:    config.ContainerSidecarControlPlane,
							Image:   config.Image,
							Command: []string{"kubevpn"},
							Args:    []string{"control-plane", "--watchDirectoryFilename", "/etc/envoy/envoy-config.yaml"},
							Ports: []v1.ContainerPort{{
								Name:          tcp9002,
								ContainerPort: 9002,
								Protocol:      v1.ProtocolTCP,
							}},
							VolumeMounts: []v1.VolumeMount{
								{
									Name:      config.VolumeEnvoyConfig,
									ReadOnly:  true,
									MountPath: "/etc/envoy",
								},
							},
							ImagePullPolicy: v1.PullIfNotPresent,
							Resources:       Resources,
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
							Env: []v1.EnvVar{
								{
									Name:  "CERT",
									Value: base64.StdEncoding.EncodeToString(cert),
								}, {
									Name:  "KEY",
									Value: base64.StdEncoding.EncodeToString(key),
								},
							},
							ImagePullPolicy: v1.PullIfNotPresent,
							Resources:       Resources,
						},
					},
					RestartPolicy:     v1.RestartPolicyAlways,
					PriorityClassName: "system-cluster-critical",
				},
			},
		},
	}
	watchStream, err := clientset.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("app", config.ConfigMapPodTrafficManager).String(),
	})
	if err != nil {
		return nil, err
	}
	defer watchStream.Stop()
	if _, err = clientset.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{}); err != nil {
		return nil, err
	}
	var last string
out:
	for {
		select {
		case e := <-watchStream.ResultChan():
			if podT, ok := e.Object.(*v1.Pod); ok {
				var sb = bytes.NewBuffer(nil)
				sb.WriteString(fmt.Sprintf("pod [%s] status is %s\n", config.ConfigMapPodTrafficManager, podT.Status.Phase))
				util.PrintStatus(podT, sb)

				if last != sb.String() {
					log.Infof(sb.String())
				}
				if podutils.IsPodReady(podT) && func() bool {
					for _, status := range podT.Status.ContainerStatuses {
						if !status.Ready {
							return false
						}
					}
					return true
				}() {
					break out
				}
				last = sb.String()
			}
		case <-time.Tick(time.Minute * 60):
			return nil, errors.New(fmt.Sprintf("wait pod %s to be ready timeout", config.ConfigMapPodTrafficManager))
		}
	}
	_, err = clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Create(ctx, &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager + "." + namespace,
			Namespace: namespace,
		},
		Webhooks: []admissionv1.MutatingWebhook{{
			Name: config.ConfigMapPodTrafficManager + ".naison.io", // 没意义的
			ClientConfig: admissionv1.WebhookClientConfig{
				Service: &admissionv1.ServiceReference{
					Namespace: namespace,
					Name:      config.ConfigMapPodTrafficManager,
					Path:      pointer.String("/pods"),
					Port:      pointer.Int32(80),
				},
				CABundle: cert,
			},
			Rules: []admissionv1.RuleWithOperations{{
				Operations: []admissionv1.OperationType{admissionv1.Create, admissionv1.Delete},
				Rule: admissionv1.Rule{
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"pods"},
					Scope:       (*admissionv1.ScopeType)(pointer.String(string(admissionv1.NamespacedScope))),
				},
			}},
			FailurePolicy: (*admissionv1.FailurePolicyType)(pointer.String(string(admissionv1.Ignore))),
			// same as above label ns
			NamespaceSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"ns": namespace}},
			SideEffects:             (*admissionv1.SideEffectClass)(pointer.String(string(admissionv1.SideEffectClassNone))),
			TimeoutSeconds:          nil,
			AdmissionReviewVersions: []string{"v1", "v1beta1"},
			ReinvocationPolicy:      (*admissionv1.ReinvocationPolicyType)(pointer.String(string(admissionv1.NeverReinvocationPolicy))),
		},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create MutatingWebhookConfigurations, err: %v", err)
	}
	_, err = updateRefCount(clientset.CoreV1().ConfigMaps(namespace), config.ConfigMapPodTrafficManager, 1)
	if err != nil {
		return
	}
	return net.ParseIP(svc.Spec.ClusterIP), nil
}

func InjectVPNSidecar(factory cmdutil.Factory, namespace, workloads string, config util.PodRouteConfig) error {
	object, err := util.GetUnstructuredObject(factory, namespace, workloads)
	if err != nil {
		return err
	}

	u := object.Object.(*unstructured.Unstructured)

	podTempSpec, path, err := util.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}

	origin := *podTempSpec

	helper := pkgresource.NewHelper(object.Client, object.Mapping)

	exchange.AddContainer(&podTempSpec.Spec, config)

	// pods without controller
	if len(path) == 0 {
		podTempSpec.Spec.PriorityClassName = ""
		for _, c := range podTempSpec.Spec.Containers {
			c.LivenessProbe = nil
			c.StartupProbe = nil
			c.ReadinessProbe = nil
		}
		p := &v1.Pod{ObjectMeta: podTempSpec.ObjectMeta, Spec: podTempSpec.Spec}
		CleanupUselessInfo(p)
		if err = createAfterDeletePod(factory, p, helper); err != nil {
			return err
		}

		RollbackFuncList = append(RollbackFuncList, func() {
			p2 := &v1.Pod{ObjectMeta: origin.ObjectMeta, Spec: origin.Spec}
			CleanupUselessInfo(p2)
			if err = createAfterDeletePod(factory, p2, helper); err != nil {
				log.Error(err)
			}
		})
	} else
	// controllers
	{
		// remove probe
		removePatch, restorePatch := patch(origin, path)
		p := []P{{
			Op:    "replace",
			Path:  "/" + strings.Join(append(path, "spec"), "/"),
			Value: podTempSpec.Spec,
		}}
		bytes, _ := json.Marshal(append(p, removePatch...))
		_, err = helper.Patch(object.Namespace, object.Name, types.JSONPatchType, bytes, &metav1.PatchOptions{})
		if err != nil {
			log.Errorf("error while inject proxy container, err: %v, exiting...", err)
			return err
		}

		RollbackFuncList = append(RollbackFuncList, func() {
			if err = removeInboundContainer(factory, namespace, workloads); err != nil {
				log.Error(err)
			}
			b, _ := json.Marshal(restorePatch)
			if _, err = helper.Patch(object.Namespace, object.Name, types.JSONPatchType, b, &metav1.PatchOptions{}); err != nil {
				log.Warnf("error while restore probe of resource: %s %s, ignore, err: %v",
					object.Mapping.GroupVersionKind.GroupKind().String(), object.Name, err)
			}
		})
	}
	_ = util.RolloutStatus(factory, namespace, workloads, time.Minute*5)
	return err
}

func createAfterDeletePod(factory cmdutil.Factory, p *v1.Pod, helper *pkgresource.Helper) error {
	if _, err := helper.DeleteWithOptions(p.Namespace, p.Name, &metav1.DeleteOptions{
		GracePeriodSeconds: pointer.Int64(0),
	}); err != nil {
		log.Error(err)
	}
	if err := retry.OnError(wait.Backoff{
		Steps:    10,
		Duration: 50 * time.Millisecond,
		Factor:   5.0,
		Jitter:   1,
	}, func(err error) bool {
		if !k8serrors.IsAlreadyExists(err) {
			return true
		}
		clientset, err := factory.KubernetesClientSet()
		get, err := clientset.CoreV1().Pods(p.Namespace).Get(context.Background(), p.Name, metav1.GetOptions{})
		if err != nil || get.Status.Phase != v1.PodRunning {
			return true
		}
		return false
	}, func() error {
		if _, err := helper.Create(p.Namespace, true, p); err != nil {
			return err
		}
		return errors.New("")
	}); err != nil {
		if k8serrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	return nil
}

func removeInboundContainer(factory cmdutil.Factory, namespace, workloads string) error {
	object, err := util.GetUnstructuredObject(factory, namespace, workloads)
	if err != nil {
		return err
	}

	u := object.Object.(*unstructured.Unstructured)

	podTempSpec, path, err := util.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}

	helper := pkgresource.NewHelper(object.Client, object.Mapping)

	// pods
	if len(path) == 0 {
		_, err = helper.DeleteWithOptions(object.Namespace, object.Name, &metav1.DeleteOptions{
			GracePeriodSeconds: pointer.Int64(0),
		})
		if err != nil {
			return err
		}
	}
	// how to scale to one
	exchange.RemoveContainer(&podTempSpec.Spec)

	bytes, err := json.Marshal([]struct {
		Op    string      `json:"op"`
		Path  string      `json:"path"`
		Value interface{} `json:"value"`
	}{{
		Op:    "replace",
		Path:  "/" + strings.Join(append(path, "spec"), "/"),
		Value: podTempSpec.Spec,
	}})
	if err != nil {
		return err
	}
	_, err = helper.Patch(object.Namespace, object.Name, types.JSONPatchType, bytes, &metav1.PatchOptions{
		//Force: &t,
	})
	return err
}

func CleanupUselessInfo(pod *v1.Pod) {
	pod.SetSelfLink("")
	pod.SetGeneration(0)
	pod.SetResourceVersion("")
	pod.SetUID("")
	pod.SetDeletionTimestamp(nil)
	pod.SetSelfLink("")
	pod.SetManagedFields(nil)
	pod.SetOwnerReferences(nil)
}

type P struct {
	Op    string      `json:"op,omitempty"`
	Path  string      `json:"path,omitempty"`
	Value interface{} `json:"value,omitempty"`
}

func patch(spec v1.PodTemplateSpec, path []string) (remove []P, restore []P) {
	for i := range spec.Spec.Containers {
		index := strconv.Itoa(i)
		readinessPath := "/" + strings.Join(append(path, "spec", "containers", index, "readinessProbe"), "/")
		livenessPath := "/" + strings.Join(append(path, "spec", "containers", index, "livenessProbe"), "/")
		startupPath := "/" + strings.Join(append(path, "spec", "containers", index, "startupProbe"), "/")
		remove = append(remove, P{
			Op:    "replace",
			Path:  readinessPath,
			Value: nil,
		}, P{
			Op:    "replace",
			Path:  livenessPath,
			Value: nil,
		}, P{
			Op:    "replace",
			Path:  startupPath,
			Value: nil,
		})
		restore = append(restore, P{
			Op:    "replace",
			Path:  readinessPath,
			Value: spec.Spec.Containers[i].ReadinessProbe,
		}, P{
			Op:    "replace",
			Path:  livenessPath,
			Value: spec.Spec.Containers[i].LivenessProbe,
		}, P{
			Op:    "replace",
			Path:  startupPath,
			Value: spec.Spec.Containers[i].StartupProbe,
		})
	}
	return
}
