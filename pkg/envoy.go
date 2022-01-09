package pkg

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/pkg/controlplane/apis/v1alpha1"
	"github.com/wencaiwulue/kubevpn/pkg/mesh"
	"github.com/wencaiwulue/kubevpn/util"
	"gopkg.in/yaml.v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	pkgresource "k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"strconv"
	"strings"
	"time"
)

// https://istio.io/latest/docs/ops/deployment/requirements/#ports-used-by-istio

//	patch a sidecar, using iptables to do port-forward let this pod decide should go to 233.254.254.100 or request to 127.0.0.1
// TODO support multiple port
func PatchSidecar(factory cmdutil.Factory, clientset *kubernetes.Clientset, namespace, workloads string, c util.PodRouteConfig, headers map[string]string) error {
	//t := true
	//zero := int64(0)
	object, err := util.GetUnstructuredObject(factory, namespace, workloads)
	if err != nil {
		return err
	}

	u := object.Object.(*unstructured.Unstructured)
	templateSpec, depth, err := util.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}

	port := uint32(templateSpec.Spec.Containers[0].Ports[0].ContainerPort)
	configMapName := fmt.Sprintf("%s-%s", object.Mapping.Resource.Resource, object.Name)

	createEnvoyConfigMapIfNeeded(clientset, object.Namespace, configMapName, strconv.Itoa(int(port)))
	err = addEnvoyConfig(clientset, object.Namespace, configMapName, c.LocalTunIP, headers, port)
	if err != nil {
		log.Warnln(err)
		return err
	}

	mesh.AddMeshContainer(templateSpec, configMapName, c)
	helper := pkgresource.NewHelper(object.Client, object.Mapping)
	bytes, err := json.Marshal([]struct {
		Op    string      `json:"op"`
		Path  string      `json:"path"`
		Value interface{} `json:"value"`
	}{{
		Op:    "replace",
		Path:  "/" + strings.Join(append(depth, "spec"), "/"),
		Value: templateSpec.Spec,
	}})
	if err != nil {
		return err
	}
	//t := true
	_, err = helper.Patch(object.Namespace, object.Name, types.JSONPatchType, bytes, &metav1.PatchOptions{
		//Force: &t,
	})

	//_ = util.WaitPod(clientset, namespace, metav1.ListOptions{
	//	FieldSelector: fields.OneTermEqualSelector("metadata.name", object.Name+"-shadow").String(),
	//}, func(pod *v1.Pod) bool {
	//	return pod.Status.Phase == v1.PodRunning
	//})
	rollbackFuncList = append(rollbackFuncList, func() {
		if err = UnPatchContainer(factory, clientset, namespace, workloads, headers); err != nil {
			log.Error(err)
		}
	})
	_ = util.RolloutStatus(factory, namespace, workloads, time.Minute*5)
	return err
}

func UnPatchContainer(factory cmdutil.Factory, clientset *kubernetes.Clientset, namespace, workloads string, headers map[string]string) error {
	//t := true
	//zero := int64(0)
	object, err := util.GetUnstructuredObject(factory, namespace, workloads)
	if err != nil {
		return err
	}

	u := object.Object.(*unstructured.Unstructured)
	templateSpec, depth, err := util.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}

	port := uint32(templateSpec.Spec.Containers[0].Ports[0].ContainerPort)
	configMapName := fmt.Sprintf("%s-%s", object.Mapping.Resource.Resource, object.Name)

	createEnvoyConfigMapIfNeeded(clientset, object.Namespace, configMapName, strconv.Itoa(int(port)))
	err = removeEnvoyConfig(clientset, object.Namespace, configMapName, headers)
	if err != nil {
		log.Warnln(err)
		return err
	}

	mesh.RemoveContainers(templateSpec)
	helper := pkgresource.NewHelper(object.Client, object.Mapping)
	bytes, err := json.Marshal([]struct {
		Op    string      `json:"op"`
		Path  string      `json:"path"`
		Value interface{} `json:"value"`
	}{{
		Op:    "replace",
		Path:  "/" + strings.Join(append(depth, "spec"), "/"),
		Value: templateSpec.Spec,
	}})
	if err != nil {
		return err
	}
	//t := true
	_, err = helper.Patch(object.Namespace, object.Name, types.JSONPatchType, bytes, &metav1.PatchOptions{
		//Force: &t,
	})

	//_ = util.WaitPod(clientset, namespace, metav1.ListOptions{
	//	FieldSelector: fields.OneTermEqualSelector("metadata.name", object.Name+"-shadow").String(),
	//}, func(pod *v1.Pod) bool {
	//	return pod.Status.Phase == v1.PodRunning
	//})
	return err
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
var ss = `name: config-test
spec:
  listeners:
  - name: listener1
    address: 127.0.0.1
    port: 15006
    routes:
    - name: route-0
      clusters:
      - cluster-0
  clusters:
  - name: cluster-0
    endpoints:
    - address: 127.0.0.1
      port: %s
`

func addEnvoyConfig(clientset *kubernetes.Clientset, namespace, workloads, localTUNIP string, headers map[string]string, port uint32) error {
	get, err := clientset.CoreV1().ConfigMaps(namespace).Get(context.TODO(), workloads, metav1.GetOptions{})
	if err != nil {
		return err
	}
	s2, ok := get.Data["envoy-config.yaml"]
	if !ok {
		return errors.New("can not found value for key: envoy-config.yaml")
	}
	envoyConfig, err := util.ParseYamlBytes([]byte(s2))
	if err != nil {
		return err
	}
	var headersMatch []v1alpha1.HeaderMatch
	for k, v := range headers {
		headersMatch = append(headersMatch, v1alpha1.HeaderMatch{
			Key:   k,
			Value: v,
		})
	}
	// move router to front
	i := len(envoyConfig.Listeners[0].Routes)
	index := strconv.Itoa(i)
	envoyConfig.Listeners[0].Routes = append(envoyConfig.Listeners[0].Routes, v1alpha1.Route{
		Name:         "route-" + index,
		Headers:      headersMatch,
		ClusterNames: []string{"cluster-" + index},
	})
	// swap last element and the last second element
	temp := envoyConfig.Listeners[0].Routes[i-1]
	envoyConfig.Listeners[0].Routes[i-1] = envoyConfig.Listeners[0].Routes[i]
	envoyConfig.Listeners[0].Routes[i] = temp

	envoyConfig.Clusters = append(envoyConfig.Clusters, v1alpha1.Cluster{
		Name: "cluster-" + index,
		Endpoints: []v1alpha1.Endpoint{{
			Address: localTUNIP,
			Port:    port,
		}},
	})
	marshal, err := yaml.Marshal(envoyConfig)
	if err != nil {
		return err
	}
	get.Data["envoy-config.yaml"] = string(marshal)
	_, err = clientset.CoreV1().ConfigMaps(namespace).Update(context.TODO(), get, metav1.UpdateOptions{})
	return err
}

func removeEnvoyConfig(clientset *kubernetes.Clientset, namespace, configMapName string, headers map[string]string) error {
	configMap, err := clientset.CoreV1().ConfigMaps(namespace).Get(context.TODO(), configMapName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	s2, ok := configMap.Data["envoy-config.yaml"]
	if !ok {
		return errors.New("can not found value for key: envoy-config.yaml")
	}
	envoyConfig, err := util.ParseYamlBytes([]byte(s2))
	if err != nil {
		return err
	}
	var routeC []v1alpha1.Route
	var name string
	var route v1alpha1.Route
	for _, route = range envoyConfig.Listeners[0].Routes {
		var m = make(map[string]string)
		for _, header := range route.Headers {
			m[header.Key] = header.Value
		}
		allMatch := true
		for k, v := range headers {
			if value, ok := m[k]; !ok || value != v {
				allMatch = false
			}
		}
		if !allMatch {
			routeC = append(routeC, route)
		} else {
			name = route.ClusterNames[0]
		}
	}
	// move router to front
	envoyConfig.Listeners[0].Routes = routeC

	var clusterC []v1alpha1.Cluster
	for _, cluster := range envoyConfig.Clusters {
		if cluster.Name != name {
			clusterC = append(clusterC, cluster)
		}
	}
	envoyConfig.Clusters = clusterC
	marshal, err := yaml.Marshal(envoyConfig)
	if err != nil {
		return err
	}
	configMap.Data["envoy-config.yaml"] = string(marshal)
	_, err = clientset.CoreV1().ConfigMaps(namespace).Update(context.TODO(), configMap, metav1.UpdateOptions{})
	return err
}

func createEnvoyConfigMapIfNeeded(clientset *kubernetes.Clientset, namespace, configMapName, port string) {
	cm, err := clientset.CoreV1().ConfigMaps(namespace).Get(context.TODO(), configMapName, metav1.GetOptions{})
	if err == nil && cm != nil {
		return
	}

	configMap := v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: namespace,
			Labels:    map[string]string{"kubevpn": "kubevpn"},
		},
		Data: map[string]string{
			"base-envoy.yaml":   fmt.Sprintf(s /*"kubevpn", podIp, port.TargetPort.String(), port.TargetPort.String()*/),
			"envoy-config.yaml": fmt.Sprintf(ss, port),
		},
	}

	_, err = clientset.CoreV1().ConfigMaps(namespace).Create(context.TODO(), &configMap, metav1.CreateOptions{})
	if err != nil {
		log.Warnln(err)
	}
}
