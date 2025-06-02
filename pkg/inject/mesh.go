package inject

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/pkg/errors"
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

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// https://istio.io/latest/docs/ops/deployment/requirements/#ports-used-by-istio

// InjectVPNAndEnvoySidecar patch a sidecar, using iptables to do port-forward let this pod decide should go to 233.254.254.100 or request to 127.0.0.1
func InjectVPNAndEnvoySidecar(ctx context.Context, nodeID string, f cmdutil.Factory, mapInterface v12.ConfigMapInterface, connectNamespace string, object *runtimeresource.Info, c util.PodRouteConfig, headers map[string]string, portMaps []string, secret *v1.Secret) (err error) {
	u := object.Object.(*unstructured.Unstructured)
	var templateSpec *v1.PodTemplateSpec
	var path []string
	templateSpec, path, err = util.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}

	var ports []controlplane.ContainerPort
	for _, container := range templateSpec.Spec.Containers {
		ports = append(ports, controlplane.ConvertContainerPort(container.Ports...)...)
	}
	for _, portMap := range portMaps {
		var found = func(containerPort int32) bool {
			for _, port := range ports {
				if port.ContainerPort == containerPort {
					return true
				}
			}
			return false
		}
		port := util.ParsePort(portMap)
		port.HostPort = 0
		if port.ContainerPort != 0 && !found(port.ContainerPort) {
			ports = append(ports, controlplane.ConvertContainerPort(port)...)
		}
	}
	var portmap = make(map[int32]string)
	for _, port := range ports {
		portmap[port.ContainerPort] = fmt.Sprintf("%d", port.ContainerPort)
	}
	for _, portMap := range portMaps {
		port := util.ParsePort(portMap)
		if port.ContainerPort != 0 {
			portmap[port.ContainerPort] = fmt.Sprintf("%d", port.HostPort)
		}
	}

	err = addEnvoyConfig(mapInterface, object.Namespace, nodeID, c, headers, ports, portmap)
	if err != nil {
		plog.G(ctx).Errorf("Failed to add envoy config: %v", err)
		return err
	}
	workload := fmt.Sprintf("%s/%s", object.Mapping.Resource.Resource, object.Name)
	// already inject container vpn and envoy-proxy, do nothing
	containerNames := sets.New[string]()
	for _, container := range templateSpec.Spec.Containers {
		containerNames.Insert(container.Name)
	}
	if containerNames.HasAll(config.ContainerSidecarVPN, config.ContainerSidecarEnvoyProxy) {
		plog.G(ctx).Infof("Workload %s/%s has already been injected with sidecar", connectNamespace, workload)
		return nil
	}

	enableIPv6, _ := util.DetectPodSupportIPv6(ctx, f, connectNamespace)
	// (1) add mesh container
	AddMeshContainer(templateSpec, object.Namespace, nodeID, c, enableIPv6, connectNamespace, secret)
	helper := pkgresource.NewHelper(object.Client, object.Mapping)
	ps := []P{
		{
			Op:    "replace",
			Path:  "/" + strings.Join(append(path, "spec"), "/"),
			Value: templateSpec.Spec,
		},
	}
	var bytes []byte
	bytes, err = k8sjson.Marshal(append(ps))
	if err != nil {
		return err
	}
	_, err = helper.Patch(object.Namespace, object.Name, types.JSONPatchType, bytes, &metav1.PatchOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Failed to patch resource: %s %s, err: %v", object.Mapping.Resource.Resource, object.Name, err)
		return err
	}
	plog.G(ctx).Infof("Patching workload %s", workload)
	err = util.RolloutStatus(ctx, f, object.Namespace, workload, time.Minute*60)
	return err
}

func UnPatchContainer(ctx context.Context, nodeID string, factory cmdutil.Factory, mapInterface v12.ConfigMapInterface, object *runtimeresource.Info, isMeFunc func(isFargateMode bool, rule *controlplane.Rule) bool) (bool, error) {
	u := object.Object.(*unstructured.Unstructured)
	templateSpec, depth, err := util.GetPodTemplateSpecPath(u)
	if err != nil {
		plog.G(ctx).Errorf("Failed to get template spec path: %v", err)
		return false, err
	}

	workload := util.ConvertUidToWorkload(nodeID)
	var empty, found bool
	empty, found, err = removeEnvoyConfig(mapInterface, object.Namespace, nodeID, isMeFunc)
	if err != nil {
		plog.G(ctx).Errorf("Failed to remove envoy config: %v", err)
		return false, err
	}
	if !found {
		plog.G(ctx).Infof("Not found proxy resource %s", workload)
		return false, nil
	}

	plog.G(ctx).Infof("Leaving workload %s", workload)

	RemoveContainers(templateSpec)

	if empty {
		helper := pkgresource.NewHelper(object.Client, object.Mapping)
		// pod without controller
		if len(depth) == 0 {
			plog.G(ctx).Debugf("Workload %s is not under controller management", workload)
			pod := &v1.Pod{ObjectMeta: templateSpec.ObjectMeta, Spec: templateSpec.Spec}
			CleanupUselessInfo(pod)
			err = CreateAfterDeletePod(ctx, factory, pod, helper)
			return empty, err
		}

		plog.G(ctx).Debugf("The %s is under controller management", workload)
		// resource with controller, like deployment,statefulset
		var bytes []byte
		bytes, err = json.Marshal([]P{
			{
				Op:    "replace",
				Path:  "/" + strings.Join(append(depth, "spec"), "/"),
				Value: templateSpec.Spec,
			},
		})
		if err != nil {
			plog.G(ctx).Errorf("Failed to generate json patch: %v", err)
			return empty, err
		}
		_, err = helper.Patch(object.Namespace, object.Name, types.JSONPatchType, bytes, &metav1.PatchOptions{})
		if err != nil {
			plog.G(ctx).Errorf("Failed to patch resource: %s %s: %v", object.Mapping.Resource.Resource, object.Name, err)
			return empty, err
		}
	}
	return empty, err
}

func addEnvoyConfig(mapInterface v12.ConfigMapInterface, ns, nodeID string, tunIP util.PodRouteConfig, headers map[string]string, port []controlplane.ContainerPort, portmap map[int32]string) error {
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

	v = addVirtualRule(v, ns, nodeID, port, headers, tunIP, portmap)
	marshal, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	configMap.Data[config.KeyEnvoy] = string(marshal)
	_, err = mapInterface.Update(context.Background(), configMap, metav1.UpdateOptions{})
	return err
}

func addVirtualRule(v []*controlplane.Virtual, ns, nodeID string, port []controlplane.ContainerPort, headers map[string]string, tunIP util.PodRouteConfig, portmap map[int32]string) []*controlplane.Virtual {
	var index = -1
	for i, virtual := range v {
		if nodeID == virtual.Uid && virtual.Namespace == ns {
			index = i
			break
		}
	}
	// 1) if not found uid, means nobody proxying it, just add it
	if index < 0 {
		return append(v, &controlplane.Virtual{
			Uid:       nodeID,
			Namespace: ns,
			Ports:     port,
			Rules: []*controlplane.Rule{{
				Headers:      headers,
				LocalTunIPv4: tunIP.LocalTunIPv4,
				LocalTunIPv6: tunIP.LocalTunIPv6,
				PortMap:      portmap,
			}},
		})
	}

	var isFargateMode bool
	for _, containerPort := range v[index].Ports {
		if containerPort.EnvoyListenerPort != 0 {
			isFargateMode = true
		}
	}
	// 2) if already proxy deployment/xxx with header foo=bar. also want to add env=dev
	if !isFargateMode {
		for j, rule := range v[index].Rules {
			if rule.LocalTunIPv4 == tunIP.LocalTunIPv4 &&
				rule.LocalTunIPv6 == tunIP.LocalTunIPv6 {
				v[index].Rules[j].Headers = util.Merge[string, string](v[index].Rules[j].Headers, headers)
				v[index].Rules[j].PortMap = util.Merge[int32, string](v[index].Rules[j].PortMap, portmap)
				return v
			}
		}
	}

	// 3) if already proxy deployment/xxx with header foo=bar, other user can replace it to self
	for j, rule := range v[index].Rules {
		if reflect.DeepEqual(rule.Headers, headers) {
			v[index].Rules[j].LocalTunIPv6 = tunIP.LocalTunIPv6
			v[index].Rules[j].LocalTunIPv4 = tunIP.LocalTunIPv4
			v[index].Rules[j].PortMap = portmap
			return v
		}
	}

	// 4) if header is not same and tunIP is not same, means another users, just add it
	v[index].Rules = append(v[index].Rules, &controlplane.Rule{
		Headers:      headers,
		LocalTunIPv4: tunIP.LocalTunIPv4,
		LocalTunIPv6: tunIP.LocalTunIPv6,
		PortMap:      portmap,
	})
	if v[index].Ports == nil {
		v[index].Ports = port
	}
	return v
}

func removeEnvoyConfig(mapInterface v12.ConfigMapInterface, namespace string, nodeID string, isMeFunc func(isFargateMode bool, rule *controlplane.Rule) bool) (empty bool, found bool, err error) {
	configMap, err := mapInterface.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		return true, false, nil
	}
	if err != nil {
		return false, false, err
	}
	str, ok := configMap.Data[config.KeyEnvoy]
	if !ok {
		return false, false, errors.New("can not found value for key: envoy-config.yaml")
	}
	var v []*controlplane.Virtual
	if err = yaml.Unmarshal([]byte(str), &v); err != nil {
		return false, false, err
	}
	for _, virtual := range v {
		if nodeID == virtual.Uid && namespace == virtual.Namespace {
			var isFargateMode bool
			for _, port := range virtual.Ports {
				if port.EnvoyListenerPort != 0 {
					isFargateMode = true
				}
			}
			for i := 0; i < len(virtual.Rules); i++ {
				if isMeFunc(isFargateMode, virtual.Rules[i]) {
					found = true
					virtual.Rules = append(virtual.Rules[:i], virtual.Rules[i+1:]...)
					i--
				}
			}
		}
	}
	if !found {
		return false, false, nil
	}

	// remove default
	for i := 0; i < len(v); i++ {
		if nodeID == v[i].Uid && namespace == v[i].Namespace && len(v[i].Rules) == 0 {
			v = append(v[:i], v[i+1:]...)
			i--
			empty = true
		}
	}
	var bytes []byte
	bytes, err = yaml.Marshal(v)
	if err != nil {
		return false, found, err
	}
	configMap.Data[config.KeyEnvoy] = string(bytes)
	_, err = mapInterface.Update(context.Background(), configMap, metav1.UpdateOptions{})
	return empty, found, err
}

func contains(a map[string]string, sub map[string]string) bool {
	for k, v := range sub {
		if a[k] != v {
			return false
		}
	}
	return true
}
