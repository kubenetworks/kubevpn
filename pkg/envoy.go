package pkg

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	config2 "github.com/wencaiwulue/kubevpn/config"
	"github.com/wencaiwulue/kubevpn/pkg/control_plane"
	"github.com/wencaiwulue/kubevpn/pkg/mesh"
	"github.com/wencaiwulue/kubevpn/util"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	pkgresource "k8s.io/cli-runtime/pkg/resource"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"sigs.k8s.io/yaml"
	"strconv"
	"strings"
	"time"
)

// https://istio.io/latest/docs/ops/deployment/requirements/#ports-used-by-istio

//	patch a sidecar, using iptables to do port-forward let this pod decide should go to 233.254.254.100 or request to 127.0.0.1
// TODO support multiple port
func PatchSidecar(factory cmdutil.Factory, clientset v12.ConfigMapInterface, namespace, workloads string, c util.PodRouteConfig, headers map[string]string) error {
	//t := true
	//zero := int64(0)
	object, err := util.GetUnstructuredObject(factory, namespace, workloads)
	if err != nil {
		return err
	}

	u := object.Object.(*unstructured.Unstructured)
	templateSpec, path, err := util.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}

	origin := *templateSpec

	var port []v1.ContainerPort
	for _, container := range templateSpec.Spec.Containers {
		port = append(port, container.Ports...)
	}
	nodeID := fmt.Sprintf("%s-%s-%s", object.Mapping.Resource.Resource, object.Mapping.Resource.Group, object.Name)

	err = addEnvoyConfig(clientset, nodeID, c.LocalTunIP, headers, port)
	if err != nil {
		log.Warnln(err)
		return err
	}

	mesh.AddMeshContainer(templateSpec, nodeID, c)
	helper := pkgresource.NewHelper(object.Client, object.Mapping)
	bytes, err := json.Marshal([]struct {
		Op    string      `json:"op"`
		Path  string      `json:"path"`
		Value interface{} `json:"value"`
	}{{
		Op:    "replace",
		Path:  "/" + strings.Join(append(path, "spec"), "/"),
		Value: templateSpec.Spec,
	}})
	if err != nil {
		return err
	}
	//t := true
	_, err = helper.Patch(object.Namespace, object.Name, types.JSONPatchType, bytes, &metav1.PatchOptions{
		//Force: &t,
	})

	removePatch, restorePatch := patch(origin, path)
	_, err = helper.Patch(object.Namespace, object.Name, types.JSONPatchType, removePatch, &metav1.PatchOptions{})
	if err != nil {
		log.Warnf("error while remove probe of resource: %s %s, ignore, err: %v",
			object.Mapping.GroupVersionKind.GroupKind().String(), object.Name, err)
	}

	rollbackFuncList = append(rollbackFuncList, func() {
		if err = UnPatchContainer(factory, clientset, namespace, workloads, headers); err != nil {
			log.Error(err)
		}
		if _, err = helper.Patch(object.Namespace, object.Name, types.JSONPatchType, restorePatch, &metav1.PatchOptions{}); err != nil {
			log.Warnf("error while restore probe of resource: %s %s, ignore, err: %v",
				object.Mapping.GroupVersionKind.GroupKind().String(), object.Name, err)
		}
	})
	_ = util.RolloutStatus(factory, namespace, workloads, time.Minute*5)
	return err
}

func UnPatchContainer(factory cmdutil.Factory, mapInterface v12.ConfigMapInterface, namespace, workloads string, headers map[string]string) error {
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

	nodeID := fmt.Sprintf("%s.%s", object.Mapping.Resource.GroupResource().String(), object.Name)

	err = removeEnvoyConfig(mapInterface, nodeID, headers)
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
	return err
}

func addEnvoyConfig(mapInterface v12.ConfigMapInterface, nodeID string, localTUNIP string, headers map[string]string, containerPorts []v1.ContainerPort) error {
	configMap, err := mapInterface.Get(context.TODO(), config2.PodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return err
	}
	var v = make([]*control_plane.EnvoyConfig, 0)
	if str, ok := configMap.Data[config2.Envoy]; ok {
		if err = yaml.Unmarshal([]byte(str), &v); err != nil {
			return err
		}
	}
	var index = -1
	for i, virtual := range v {
		if nodeID == virtual.NodeID {
			index = i
		}
	}
	var h []control_plane.HeaderMatch
	for k, v := range headers {
		h = append(h, control_plane.HeaderMatch{Key: k, Value: v})
	}
	var l []control_plane.ListenerTemp
	var c []control_plane.ClusterTemp
	// if we can't find nodeID, just add it
	if index < 0 {
		for _, port := range containerPorts {
			clusterName := localTUNIP + "_" + strconv.Itoa(int(port.ContainerPort))
			c = append(c, control_plane.ClusterTemp{
				Name:      clusterName,
				Endpoints: []control_plane.EndpointTemp{{Address: localTUNIP, Port: uint32(port.ContainerPort)}},
			})
			l = append(l, control_plane.ListenerTemp{
				Name:    strconv.Itoa(int(port.ContainerPort)),
				Address: "0.0.0.0",
				Port:    uint32(port.ContainerPort),
				Routes: []control_plane.RouteTemp{
					{
						Headers:     h,
						ClusterName: clusterName,
					},
					{
						Headers:     nil,
						ClusterName: "origin_cluster",
					},
				},
			})
		}
		v = append(v, &control_plane.EnvoyConfig{
			NodeID: nodeID,
			Spec: control_plane.Spec{
				Listeners: l,
				Clusters:  c,
			},
		})
	} else {
		// if listener already exist, needs to add route
		// if not exist, needs to create this listener, and then add route
		// make sure position of default route is last

		for _, port := range containerPorts {
			clusterName := localTUNIP + "_" + strconv.Itoa(int(port.ContainerPort))
			for _, listener := range v[index].Spec.Listeners {
				if listener.Port == uint32(port.ContainerPort) {
					listener.Routes = append(
						[]control_plane.RouteTemp{{Headers: h, ClusterName: clusterName}}, listener.Routes...,
					)
				}
			}
			var found = false
			for _, cluster := range v[index].Spec.Clusters {
				if cluster.Name == clusterName {
					found = true
					break
				}
			}
			if !found {
				v[index].Spec.Clusters = append(v[index].Spec.Clusters, control_plane.ClusterTemp{
					Name: clusterName,
					Endpoints: []control_plane.EndpointTemp{{
						Address: localTUNIP,
						Port:    uint32(port.ContainerPort),
					}},
				})
			}
		}
	}

	marshal, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	configMap.Data[config2.Envoy] = string(marshal)
	_, err = mapInterface.Update(context.Background(), configMap, metav1.UpdateOptions{})
	return err
}

func removeEnvoyConfig(mapInterface v12.ConfigMapInterface, nodeID string, headers map[string]string) error {
	configMap, err := mapInterface.Get(context.TODO(), config2.PodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return err
	}
	str, ok := configMap.Data[config2.Envoy]
	if !ok {
		return errors.New("can not found value for key: envoy-config.yaml")
	}
	var v []*control_plane.Virtual
	if err = yaml.Unmarshal([]byte(str), &v); err != nil {
		return err
	}
	for _, virtual := range v {
		if nodeID == virtual.Uid {
			for i := 0; i < len(virtual.Rules); i++ {
				if contains(virtual.Rules[i].Headers, headers) {
					virtual.Rules = append(virtual.Rules[:i], virtual.Rules[i+1:]...)
					i--
				}
			}
		}
	}
	marshal, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	configMap.Data[config2.Envoy] = string(marshal)
	_, err = mapInterface.Update(context.Background(), configMap, metav1.UpdateOptions{})
	return err
}

func contains(a map[string]string, sub map[string]string) bool {
	for k, v := range sub {
		if a[k] != v {
			return false
		}
	}
	return true
}
