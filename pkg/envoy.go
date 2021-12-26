package pkg

import (
	"context"
	"fmt"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/pkg/mesh"
	"github.com/wencaiwulue/kubevpn/util"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"strings"
)

// https://istio.io/latest/docs/ops/deployment/requirements/#ports-used-by-istio

//	patch a sidecar, using iptables to do port-forward let this pod decide should go to 233.254.254.100 or request to 127.0.0.1
// TODO if using envoy needs to create another pod, if using diy proxy, using one container is enough
// TODO support multiple port
func PatchSidecar(factory cmdutil.Factory, clientset *kubernetes.Clientset, namespace, workloads string, c PodRouteConfig) error {
	resourceTuple, parsed, err2 := util.SplitResourceTypeName(workloads)
	if !parsed || err2 != nil {
		return errors.New("not need")
	}
	t := true
	zero := int64(0)
	var sc mesh.Injectable
	switch strings.ToLower(resourceTuple.Resource) {
	case "deployment", "deployments":
		sc = mesh.NewDeploymentController(factory, clientset, namespace, resourceTuple.Name)
	case "statefulset", "statefulsets":
		sc = mesh.NewStatefulsetController(factory, clientset, namespace, resourceTuple.Name)
	case "replicaset", "replicasets":
		sc = mesh.NewReplicasController(factory, clientset, namespace, resourceTuple.Name)
	case "service", "services":
		sc = mesh.NewServiceController(factory, clientset, namespace, resourceTuple.Name)
	case "pod", "pods":
		sc = mesh.NewPodController(factory, clientset, namespace, "pods", resourceTuple.Name)
	default:
		sc = mesh.NewPodController(factory, clientset, namespace, resourceTuple.Resource, resourceTuple.Name)
	}
	rollbackFuncs = append(rollbackFuncs, func() {
		if err := sc.Cancel(); err != nil {
			log.Warnln(err)
		}
	})

	labels, inject, err := sc.Inject()
	if err != nil {
		return err
	}
	delete(labels, "pod-template-hash")

	name := fmt.Sprintf("%s-%s", namespace, resourceTuple.Name)
	createEnvoyConfigMapIfNeeded(factory, clientset, namespace, workloads, c.LocalTunIP)
	inject.Volumes = append(inject.Volumes, v1.Volume{
		Name: "envoy-config",
		VolumeSource: v1.VolumeSource{
			ConfigMap: &v1.ConfigMapVolumeSource{
				LocalObjectReference: v1.LocalObjectReference{
					Name: name,
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
	inject.Containers = append(inject.Containers, v1.Container{
		Name:    "vpn",
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
	inject.Containers = append(inject.Containers, v1.Container{
		Name:    "envoy-proxy",
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
				Name:      "envoy-config",
				ReadOnly:  false,
				MountPath: "/etc/envoy/",
				//SubPath:   "envoy.yaml",
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

var s = `
static_resources:
  clusters:
    - connect_timeout: 1s
      load_assignment:
        cluster_name: xds_cluster
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: 127.0.0.1
                      port_value: 9002
      http2_protocol_options: {}
      name: xds_cluster
dynamic_resources:
  cds_config:
    resource_api_version: V3
    api_config_source:
      api_type: GRPC
      transport_api_version: V3
      grpc_services:
        - envoy_grpc:
            cluster_name: xds_cluster
      set_node_on_first_message_only: true
  lds_config:
    resource_api_version: V3
    api_config_source:
      api_type: GRPC
      transport_api_version: V3
      grpc_services:
        - envoy_grpc:
            cluster_name: xds_cluster
      set_node_on_first_message_only: true
node:
  cluster: test-cluster
  id: test-id
layered_runtime:
  layers:
    - name: runtime-0
      rtds_layer:
        rtds_config:
          resource_api_version: V3
          api_config_source:
            transport_api_version: V3
            api_type: GRPC
            grpc_services:
              envoy_grpc:
                cluster_name: xds_cluster
        name: runtime-0
admin:
  access_log_path: /dev/null
  address:
    socket_address:
      address: 127.0.0.1
      port_value: 9003

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
	//port := serviceList.Items[0].Spec.Ports[0]
	configMap := v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"kubevpn": "kubevpn"},
		},
		Data: map[string]string{
			"base-envoy.yaml":   fmt.Sprintf(s, /*"kubevpn", podIp, port.TargetPort.String(), port.TargetPort.String()*/),
			"envoy-config.yaml": "",
		},
	}
	_ = clientset.CoreV1().ConfigMaps(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		_, err = clientset.CoreV1().ConfigMaps(namespace).Create(context.TODO(), &configMap, metav1.CreateOptions{})
		return err
	})
	if err != nil {
		log.Warnln(err)
	}
}
