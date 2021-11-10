package remote

import (
	"context"
	"fmt"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/util"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"strings"
	"time"
)

// https://istio.io/latest/docs/ops/deployment/requirements/#ports-used-by-istio

//	patch a sidecar, using iptables to do port-forward let this pod decide should go to 233.254.254.100 or request to 127.0.0.1
// TODO if using envoy needs to create another pod, if using diy proxy, using one container is enough
// TODO support multiple port
func PatchSidecar(factory cmdutil.Factory, clientset *kubernetes.Clientset, namespace, workloads, virtualLocalIp, realRouterIP, virtualShadowIp, routes string) error {
	// create pod in bound for mesh
	err, podIp := CreateServerInboundForMesh(clientset, namespace, workloads, virtualLocalIp, realRouterIP, virtualShadowIp, routes)
	if err != nil {
		log.Warnln(err)
		return err
	}
	log.Infof(podIp)
	resourceTuple, parsed, err2 := util.SplitResourceTypeName(workloads)
	if !parsed || err2 != nil {
		return errors.New("not need")
	}
	t := true
	zero := int64(0)
	var sc Injectable
	switch strings.ToLower(resourceTuple.Resource) {
	case "deployment", "deployments":
		sc = NewDeploymentController(factory, clientset, namespace, resourceTuple.Name)
	case "statefulset", "statefulsets":
		sc = NewStatefulsetController(factory, clientset, namespace, resourceTuple.Name)
	case "replicaset", "replicasets":
		sc = NewReplicasController(factory, clientset, namespace, resourceTuple.Name)
	case "service", "services":
		sc = NewServiceController(factory, clientset, namespace, resourceTuple.Name)
	case "pod", "pods":
		sc = NewPodController(factory, clientset, namespace, "pods", resourceTuple.Name)
	default:
		sc = NewPodController(factory, clientset, namespace, resourceTuple.Resource, resourceTuple.Name)
	}
	CancelFunctions = append(CancelFunctions, func() {
		if err = sc.Cancel(); err != nil {
			log.Warnln(err)
		}
	})

	labels, inject, err := sc.Inject()
	if err != nil {
		return err
	}
	delete(labels, "pod-template-hash")

	name := fmt.Sprintf("%s-%s", namespace, resourceTuple.Name)
	createEnvoyConfigMapIfNeeded(factory, clientset, namespace, workloads, podIp)
	inject.Volumes = append(inject.Volumes, v1.Volume{
		Name: "envoy-config",
		VolumeSource: v1.VolumeSource{
			ConfigMap: &v1.ConfigMapVolumeSource{
				LocalObjectReference: v1.LocalObjectReference{
					Name: name,
				},
			},
		},
	})
	inject.Containers = append(inject.Containers, v1.Container{
		Name:    "envoy-proxy",
		Image:   "naison/kubevpnmesh:v2",
		Command: []string{"/bin/sh", "-c"},
		Args: []string{
			"sysctl net.ipv4.ip_forward=1;" +
				"iptables -F;" +
				"iptables -P INPUT ACCEPT;" +
				"iptables -P FORWARD ACCEPT;" +
				"iptables -t nat -A PREROUTING -i eth0 -p tcp --dport 80:60000 ! -s 127.0.0.1 -j DNAT --to 127.0.0.1:15006;" +
				"iptables -t nat -A POSTROUTING -p tcp -m tcp --dport 80:60000 ! -s 127.0.0.1 -j MASQUERADE;" +
				"iptables -t nat -A PREROUTING -i eth0 -p udp --dport 80:60000 ! -s 127.0.0.1 -j DNAT --to 127.0.0.1:15006;" +
				"iptables -t nat -A POSTROUTING -p udp -m udp --dport 80:60000 ! -s 127.0.0.1 -j MASQUERADE;" +
				"envoy -c /etc/envoy.yaml",
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
				Name:      "envoy-config",
				ReadOnly:  false,
				MountPath: "/etc/envoy.yaml",
				SubPath:   "envoy.yaml",
			},
		},
	})
	if err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		_, err = clientset.CoreV1().Pods(namespace).Create(context.TODO(), &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      resourceTuple.Name + "-shadow",
				Namespace: namespace,
				Labels:    labels,
			},
			Spec: *inject,
		}, metav1.CreateOptions{})
		return err
	}); err != nil {
		return err
	}
	_ = util.WaitPod(clientset, namespace, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", resourceTuple.Name+"-shadow").String(),
	}, func(pod *v1.Pod) bool {
		return pod.Status.Phase == v1.PodRunning
	})
	return nil
}

var s = `static_resources:

  listeners:
    - name: listener_0
      address:
        socket_address:
          address: 0.0.0.0
          port_value: 15006
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: ingress_http
                access_log:
                  - name: envoy.access_loggers.stdout
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.access_loggers.stream.v3.StdoutAccessLog
                http_filters:
                  - name: envoy.filters.http.router
                route_config:
                  name: local_route
                  virtual_hosts:
                    - name: local_service
                      domains: ["*"]
                      routes:
                        - match:
                            headers:
                              - name: KubeVPN-Routing-Tag
                                exact_match: %s
                            prefix: "/"
                          route:
                            # host_rewrite_literal: www.envoyproxy.io
                            cluster: service_debug_withHeader
                        - match:
                            prefix: "/"
                          route:
                            # host_rewrite_literal: www.envoyproxy.io
                            cluster: service_debug_withoutHeader

  clusters:
    - name: service_debug_withHeader
      type: LOGICAL_DNS
      # Comment out the following line to test on v6 networks
      dns_lookup_family: V4_ONLY
      load_assignment:
        cluster_name: service_debug_withHeader
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: %s
                      port_value: %s
    - name: service_debug_withoutHeader
      type: LOGICAL_DNS
      # Comment out the following line to test on v6 networks
      dns_lookup_family: V4_ONLY
      load_assignment:
        cluster_name: service_debug_withoutHeader
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: 127.0.0.1
                      port_value: %s
`

func createEnvoyConfigMapIfNeeded(factory cmdutil.Factory, clientset *kubernetes.Clientset, namespace, workloads, podIp string) {
	resourceTuple, parsed, err2 := util.SplitResourceTypeName(workloads)
	if !parsed || err2 != nil {
		return
	}
	name := fmt.Sprintf("%s-%s", namespace, resourceTuple.Name)
	object, err := util.GetUnstructuredObject(factory, namespace, workloads)
	if err != nil {
		return
	}
	asSelector, _ := metav1.LabelSelectorAsSelector(util.GetLabelSelector(object.Object))
	serviceList, _ := clientset.CoreV1().Services(namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: asSelector.String(),
	})
	if len(serviceList.Items) == 0 {
		return
	}
	port := serviceList.Items[0].Spec.Ports[0]
	configMap := v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"kubevpn": "kubevpn"},
		},
		Data: map[string]string{"envoy.yaml": fmt.Sprintf(s, "kubevpn", podIp, port.TargetPort.String(), port.TargetPort.String())},
	}
	_ = clientset.CoreV1().ConfigMaps(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		_, err := clientset.CoreV1().ConfigMaps(namespace).Create(context.TODO(), &configMap, metav1.CreateOptions{})
		return err
	})
	if err != nil {
		log.Warnln(err)
	}
}

func CreateServerInboundForMesh(clientset *kubernetes.Clientset, namespace, workloads, virtualLocalIp, realRouterIP, virtualShadowIp, routes string) (error, string) {
	resourceTuple, parsed, err2 := util.SplitResourceTypeName(workloads)
	if !parsed || err2 != nil {
		return errors.New("not need"), ""
	}
	newName := resourceTuple.Name + "-shadow-mesh"
	util.DeletePod(clientset, namespace, newName)
	t := true
	zero := int64(0)
	pod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      newName,
			Namespace: namespace,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    "vpn",
					Image:   "naison/kubevpn:v2",
					Command: []string{"/bin/sh", "-c"},
					Args: []string{
						"sysctl net.ipv4.ip_forward=1;" +
							"iptables -F;" +
							"iptables -P INPUT ACCEPT;" +
							"iptables -P FORWARD ACCEPT;" +
							"iptables -t nat -A PREROUTING -i eth0 -p tcp --dport 80:60000 -j DNAT --to " + virtualLocalIp + ":80-60000;" +
							"iptables -t nat -A POSTROUTING -p tcp -m tcp --dport 80:60000 -j MASQUERADE;" +
							"iptables -t nat -A PREROUTING -i eth0 -p udp --dport 80:60000 -j DNAT --to " + virtualLocalIp + ":80-60000;" +
							"iptables -t nat -A POSTROUTING -p udp -m udp --dport 80:60000 -j MASQUERADE;" +
							"kubevpn serve -L 'tun://0.0.0.0:8421/" + realRouterIP + ":8421?net=" + virtualShadowIp + "&route=" + routes + "' --debug=true",
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
				},
			},
			PriorityClassName: "system-cluster-critical",
		},
	}
	if _, err := clientset.CoreV1().Pods(namespace).Create(context.TODO(), &pod, metav1.CreateOptions{}); err != nil {
		log.Fatal(err)
	}
	watch, err := clientset.CoreV1().Pods(namespace).Watch(context.TODO(), metav1.SingleObject(metav1.ObjectMeta{Name: newName}))
	if err != nil {
		log.Fatal(err)
	}
	tick := time.Tick(time.Minute * 2)
	for {
		select {
		case e := <-watch.ResultChan():
			if e.Object.(*v1.Pod).Status.Phase == v1.PodRunning {
				watch.Stop()
				return nil, e.Object.(*v1.Pod).Status.PodIP
			}
		case <-tick:
			watch.Stop()
			log.Error("create mesh inbound timeout")
			return errors.New("create inbound mesh timeout"), ""
		}
	}
}
