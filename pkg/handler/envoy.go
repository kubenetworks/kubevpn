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
	k8sjson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/sets"
	pkgresource "k8s.io/cli-runtime/pkg/resource"
	runtimeresource "k8s.io/cli-runtime/pkg/resource"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/pkg/mesh"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

// https://istio.io/latest/docs/ops/deployment/requirements/#ports-used-by-istio

// InjectVPNAndEnvoySidecar patch a sidecar, using iptables to do port-forward let this pod decide should go to 233.254.254.100 or request to 127.0.0.1
func InjectVPNAndEnvoySidecar(ctx1 context.Context, factory cmdutil.Factory, clientset v12.ConfigMapInterface, namespace, workload string, c util.PodRouteConfig, headers map[string]string) (err error) {
	var object *runtimeresource.Info
	object, err = util.GetUnstructuredObject(factory, namespace, workload)
	if err != nil {
		return err
	}

	u := object.Object.(*unstructured.Unstructured)
	var templateSpec *v1.PodTemplateSpec
	var path []string
	templateSpec, path, err = util.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}

	origin := templateSpec.DeepCopy()

	var port []v1.ContainerPort
	for _, container := range templateSpec.Spec.Containers {
		port = append(port, container.Ports...)
	}
	nodeID := fmt.Sprintf("%s.%s", object.Mapping.Resource.GroupResource().String(), object.Name)

	err = addEnvoyConfig(clientset, nodeID, c, headers, port)
	if err != nil {
		log.Warnln(err)
		return err
	}

	// already inject container vpn and envoy-proxy, do nothing
	containerNames := sets.New[string]()
	for _, container := range templateSpec.Spec.Containers {
		containerNames.Insert(container.Name)
	}
	if containerNames.HasAll(config.ContainerSidecarVPN, config.ContainerSidecarEnvoyProxy) {
		// add rollback func to remove envoy config
		//RollbackFuncList = append(RollbackFuncList, func() {
		//	err := UnPatchContainer(factory, clientset, namespace, workload, c.LocalTunIPv4)
		//	if err != nil {
		//		log.Error(err)
		//	}
		//})
		return nil
	}
	// (1) add mesh container
	removePatch, restorePatch := patch(*origin, path)
	var b []byte
	b, err = k8sjson.Marshal(restorePatch)
	if err != nil {
		return err
	}

	mesh.AddMeshContainer(templateSpec, nodeID, c)
	helper := pkgresource.NewHelper(object.Client, object.Mapping)
	ps := []P{
		{
			Op:    "replace",
			Path:  "/" + strings.Join(append(path, "spec"), "/"),
			Value: templateSpec.Spec,
		},
		{
			Op:    "replace",
			Path:  "/metadata/annotations/" + config.KubeVPNRestorePatchKey,
			Value: string(b),
		},
	}
	var bytes []byte
	bytes, err = k8sjson.Marshal(append(ps, removePatch...))
	if err != nil {
		return err
	}
	_, err = helper.Patch(object.Namespace, object.Name, types.JSONPatchType, bytes, &metav1.PatchOptions{})
	if err != nil {
		log.Warnf("error while path resource: %s %s, err: %v", object.Mapping.GroupVersionKind.GroupKind().String(), object.Name, err)
		return err
	}

	//RollbackFuncList = append(RollbackFuncList, func() {
	//	if err := UnPatchContainer(factory, clientset, namespace, workload, c.LocalTunIPv4); err != nil {
	//		log.Error(err)
	//	}
	//})
	if err != nil {
		return err
	}
	err = util.RolloutStatus(ctx1, factory, namespace, workload, time.Minute*60)
	return err
}

func UnPatchContainer(factory cmdutil.Factory, mapInterface v12.ConfigMapInterface, namespace, workload string, localTunIPv4 string) error {
	object, err := util.GetUnstructuredObject(factory, namespace, workload)
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
	empty, err = removeEnvoyConfig(mapInterface, nodeID, localTunIPv4)
	if err != nil {
		log.Warnln(err)
		return err
	}

	mesh.RemoveContainers(templateSpec)
	if u.GetAnnotations() != nil && u.GetAnnotations()[config.KubeVPNRestorePatchKey] != "" {
		patchStr := u.GetAnnotations()[config.KubeVPNRestorePatchKey]
		var ps []P
		err = json.Unmarshal([]byte(patchStr), &ps)
		if err != nil {
			return fmt.Errorf("unmarshal json patch: %s failed, err: %v", patchStr, err)
		}
		fromPatchToProbe(templateSpec, depth, ps)
	}

	if empty {
		helper := pkgresource.NewHelper(object.Client, object.Mapping)
		// pod without controller
		if len(depth) == 0 {
			delete(templateSpec.ObjectMeta.GetAnnotations(), config.KubeVPNRestorePatchKey)
			pod := &v1.Pod{ObjectMeta: templateSpec.ObjectMeta, Spec: templateSpec.Spec}
			CleanupUselessInfo(pod)
			err = CreateAfterDeletePod(factory, pod, helper)
			return err
		}

		// resource with controller, like deployment,statefulset
		var bytes []byte
		bytes, err = json.Marshal([]P{
			{
				Op:    "replace",
				Path:  "/" + strings.Join(append(depth, "spec"), "/"),
				Value: templateSpec.Spec,
			},
			{
				Op:    "replace",
				Path:  "/metadata/annotations/" + config.KubeVPNRestorePatchKey,
				Value: "",
			},
		})
		if err != nil {
			return err
		}
		_, err = helper.Patch(object.Namespace, object.Name, types.JSONPatchType, bytes, &metav1.PatchOptions{})
		if err != nil {
			return err
		}
	}
	return err
}

func addEnvoyConfig(mapInterface v12.ConfigMapInterface, nodeID string, tunIP util.PodRouteConfig, headers map[string]string, port []v1.ContainerPort) error {
	configMap, err := mapInterface.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
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
				Headers:      headers,
				LocalTunIPv4: tunIP.LocalTunIPv4,
				LocalTunIPv6: tunIP.LocalTunIPv6,
			}},
		})
	} else {
		var found bool
		for j, rule := range v[index].Rules {
			if rule.LocalTunIPv4 == tunIP.LocalTunIPv4 &&
				rule.LocalTunIPv6 == tunIP.LocalTunIPv6 {
				found = true
				v[index].Rules[j].Headers = util.Merge[string, string](v[index].Rules[j].Headers, headers)
			}
		}
		if !found {
			v[index].Rules = append(v[index].Rules, &controlplane.Rule{
				Headers:      headers,
				LocalTunIPv4: tunIP.LocalTunIPv4,
				LocalTunIPv6: tunIP.LocalTunIPv6,
			})
			if v[index].Ports == nil {
				v[index].Ports = port
			}
		}
	}

	marshal, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	configMap.Data[config.KeyEnvoy] = string(marshal)
	_, err = mapInterface.Update(context.Background(), configMap, metav1.UpdateOptions{})
	return err
}

func removeEnvoyConfig(mapInterface v12.ConfigMapInterface, nodeID string, localTunIPv4 string) (bool, error) {
	configMap, err := mapInterface.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
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
				if virtual.Rules[i].LocalTunIPv4 == localTunIPv4 {
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
	var bytes []byte
	bytes, err = yaml.Marshal(v)
	if err != nil {
		return false, err
	}
	configMap.Data[config.KeyEnvoy] = string(bytes)
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
