package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	pkgresource "k8s.io/cli-runtime/pkg/resource"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/pkg/mesh"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

// https://istio.io/latest/docs/ops/deployment/requirements/#ports-used-by-istio

//	patch a sidecar, using iptables to do port-forward let this pod decide should go to 233.254.254.100 or request to 127.0.0.1
func InjectVPNAndEnvoySidecar(factory cmdutil.Factory, clientset v12.ConfigMapInterface, namespace, workloads string, c util.PodRouteConfig, headers map[string]string) error {
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
	nodeID := fmt.Sprintf("%s.%s", object.Mapping.Resource.GroupResource().String(), object.Name)

	err = addEnvoyConfig(clientset, nodeID, c.LocalTunIP, headers, port)
	if err != nil {
		log.Warnln(err)
		return err
	}

	// already inject container vpn and envoy-proxy, do nothing
	containerNames := sets.NewString()
	for _, container := range templateSpec.Spec.Containers {
		containerNames.Insert(container.Name)
	}
	if containerNames.HasAll(config.ContainerSidecarVPN, config.ContainerSidecarEnvoyProxy) {
		// add rollback func to remove envoy config
		RollbackFuncList = append(RollbackFuncList, func() {
			err := UnPatchContainer(factory, clientset, namespace, workloads, headers)
			if err != nil {
				log.Error(err)
			}
		})
		return nil
	}
	// (1) add mesh container
	removePatch, restorePatch := patch(origin, path)
	b, _ := json.Marshal(restorePatch)
	mesh.AddMeshContainer(templateSpec, nodeID, c)
	helper := pkgresource.NewHelper(object.Client, object.Mapping)
	ps := []P{{
		Op:    "replace",
		Path:  "/" + strings.Join(append(path, "spec"), "/"),
		Value: templateSpec.Spec,
	}, {
		Op:    "replace",
		Path:  "/metadata/annotations/probe",
		Value: b,
	}}
	bytes, err := json.Marshal(append(ps, removePatch...))
	if err != nil {
		return err
	}
	_, err = helper.Patch(object.Namespace, object.Name, types.JSONPatchType, bytes, &metav1.PatchOptions{})
	if err != nil {
		log.Warnf("error while remove probe of resource: %s %s, ignore, err: %v", object.Mapping.GroupVersionKind.GroupKind().String(), object.Name, err)
	}

	RollbackFuncList = append(RollbackFuncList, func() {
		if err = UnPatchContainer(factory, clientset, namespace, workloads, headers); err != nil {
			log.Error(err)
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

	var empty bool
	empty, err = removeEnvoyConfig(mapInterface, nodeID, headers)
	if err != nil {
		log.Warnln(err)
		return err
	}

	if empty {
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
		}, {
			Op:    "replace",
			Path:  "/metadata/annotations/probe",
			Value: "",
		}})
		if err != nil {
			return err
		}
		_, err = helper.Patch(object.Namespace, object.Name, types.JSONPatchType, bytes, &metav1.PatchOptions{})
	}
	return err
}

func addEnvoyConfig(mapInterface v12.ConfigMapInterface, nodeID string, localTUNIP string, headers map[string]string, port []v1.ContainerPort) error {
	configMap, err := mapInterface.Get(context.TODO(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return err
	}
	var v = make([]*controlplane.Virtual, 0)
	if str, ok := configMap.Data[config.KeyEnvoy]; ok {
		if err = yaml.Unmarshal([]byte(str), &v); err != nil {
			return err
		}
	}
	var index = -1
	for i, virtual := range v {
		if nodeID == virtual.Uid {
			index = i
			break
		}
	}
	if index < 0 {
		v = append(v, &controlplane.Virtual{
			Uid:   nodeID,
			Ports: port,
			Rules: []*controlplane.Rule{{
				Headers:    headers,
				LocalTunIP: localTUNIP,
			}},
		})
	} else {
		v[index].Rules = append(v[index].Rules, &controlplane.Rule{
			Headers:    headers,
			LocalTunIP: localTUNIP,
		})
	}

	marshal, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	configMap.Data[config.KeyEnvoy] = string(marshal)
	_, err = mapInterface.Update(context.Background(), configMap, metav1.UpdateOptions{})
	return err
}

func removeEnvoyConfig(mapInterface v12.ConfigMapInterface, nodeID string, headers map[string]string) (bool, error) {
	configMap, err := mapInterface.Get(context.TODO(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	str, ok := configMap.Data[config.KeyEnvoy]
	if !ok {
		return false, errors.New("can not found value for key: envoy-config.yaml")
	}
	var v []*controlplane.Virtual
	if err = yaml.Unmarshal([]byte(str), &v); err != nil {
		return false, err
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
	var empty bool
	// remove default
	for i := 0; i < len(v); i++ {
		if nodeID == v[i].Uid && len(v[i].Rules) == 0 {
			v = append(v[:i], v[i+1:]...)
			i--
			empty = true
		}
	}
	marshal, err := yaml.Marshal(v)
	if err != nil {
		return false, err
	}
	configMap.Data[config.KeyEnvoy] = string(marshal)
	_, err = mapInterface.Update(context.Background(), configMap, metav1.UpdateOptions{})
	return empty, err
}

func contains(a map[string]string, sub map[string]string) bool {
	for k, v := range sub {
		if a[k] != v {
			return false
		}
	}
	return true
}
