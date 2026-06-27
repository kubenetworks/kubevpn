package inject

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"maps"
	"sort"
	"text/template"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

//go:embed envoy.yaml
var envoyConfig []byte

//go:embed envoy_ipv4.yaml
var envoyConfigIPv4 []byte

//go:embed fargate_envoy.yaml
var envoyConfigFargate []byte

//go:embed fargate_envoy_ipv4.yaml
var envoyConfigIPv4Fargate []byte

func renderEnvoyConfig(tmplStr string, value string) string {
	tmpl, err := template.New("").Parse(tmplStr)
	if err != nil {
		return ""
	}
	buf := new(bytes.Buffer)
	err = tmpl.Execute(buf, value)
	if err != nil {
		return ""
	}
	return buf.String()
}

func addEnvoyConfig(ctx context.Context, mapInterface v12.ConfigMapInterface, ns, nodeID string, localTunIPv4, localTunIPv6 string, headers map[string]string, port []controlplane.ContainerPort, portmap map[int32]string, fargateMode bool) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		configMap, err := mapInterface.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if err != nil {
			return err
		}
		var v = make([]*controlplane.Virtual, 0)
		if str, ok := configMap.Data[config.KeyEnvoy]; ok {
			if err = yaml.Unmarshal([]byte(str), &v); err != nil {
				return err
			}
		}

		v = addVirtualRule(v, ns, nodeID, port, headers, localTunIPv4, localTunIPv6, portmap, fargateMode)
		marshal, err := yaml.Marshal(v)
		if err != nil {
			return err
		}
		configMap.Data[config.KeyEnvoy] = string(marshal)
		_, err = mapInterface.Update(ctx, configMap, metav1.UpdateOptions{})
		return err
	})
}

func addVirtualRule(v []*controlplane.Virtual, ns, nodeID string, port []controlplane.ContainerPort, headers map[string]string, localTunIPv4, localTunIPv6 string, portmap map[int32]string, fargateMode bool) []*controlplane.Virtual {
	var index = -1
	for i, virtual := range v {
		if nodeID == virtual.UID && virtual.Namespace == ns {
			index = i
			break
		}
	}
	// 1) if not found uid, means nobody proxying it, just add it
	if index < 0 {
		return append(v, &controlplane.Virtual{
			UID:         nodeID,
			Namespace:   ns,
			FargateMode: fargateMode,
			Ports:       port,
			Rules: []*controlplane.Rule{{
				Headers:      headers,
				LocalTunIPv4: localTunIPv4,
				LocalTunIPv6: localTunIPv6,
				PortMap:      portmap,
			}},
		})
	}

	// 2) if already proxy deployment/xxx with header foo=bar. also want to add env=dev
	if !v[index].IsFargateMode() {
		for j, rule := range v[index].Rules {
			if rule.LocalTunIPv4 == localTunIPv4 &&
				rule.LocalTunIPv6 == localTunIPv6 {
				v[index].Rules[j].Headers = util.Merge[string, string](v[index].Rules[j].Headers, headers)
				v[index].Rules[j].PortMap = util.Merge[int32, string](v[index].Rules[j].PortMap, portmap)
				return v
			}
		}
	}

	// 3) if already proxy deployment/xxx with header foo=bar, other user can replace it to self
	for j, rule := range v[index].Rules {
		if maps.Equal(rule.Headers, headers) {
			v[index].Rules[j].LocalTunIPv6 = localTunIPv6
			v[index].Rules[j].LocalTunIPv4 = localTunIPv4
			v[index].Rules[j].PortMap = portmap
			return v
		}
	}

	// 4) if header is not same and tunIP is not same, means another users, just add it
	v[index].Rules = append(v[index].Rules, &controlplane.Rule{
		Headers:      headers,
		LocalTunIPv4: localTunIPv4,
		LocalTunIPv6: localTunIPv6,
		PortMap:      portmap,
	})
	if v[index].Ports == nil {
		v[index].Ports = port
	}

	// envoy rule have order, eg:
	// 1. null header to a
	// 2. foo=bar to b
	// then will never hit to b
	// so needs to let null header to last rule
	for x := range v {
		sort.SliceStable(v[x].Rules, func(i, j int) bool {
			return len(v[x].Rules[i].Headers) != 0
		})
	}
	return v
}

func removeEnvoyConfig(ctx context.Context, mapInterface v12.ConfigMapInterface, namespace string, nodeID string, isMeFunc func(isFargateMode bool, rule *controlplane.Rule) bool) (empty bool, found bool, err error) {
	configMap, err := mapInterface.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		return true, false, nil
	}
	if err != nil {
		return false, false, err
	}
	str, ok := configMap.Data[config.KeyEnvoy]
	if !ok {
		return false, false, fmt.Errorf("cannot find value for key: envoy-config.yaml")
	}
	var v []*controlplane.Virtual
	if err = yaml.Unmarshal([]byte(str), &v); err != nil {
		return false, false, err
	}
	for _, virtual := range v {
		if nodeID == virtual.UID && namespace == virtual.Namespace {
			for i := 0; i < len(virtual.Rules); i++ {
				if isMeFunc(virtual.IsFargateMode(), virtual.Rules[i]) {
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
		if nodeID == v[i].UID && namespace == v[i].Namespace && len(v[i].Rules) == 0 {
			v = append(v[:i], v[i+1:]...)
			i--
			empty = true
		}
	}
	var b []byte
	b, err = yaml.Marshal(v)
	if err != nil {
		return false, found, err
	}
	configMap.Data[config.KeyEnvoy] = string(b)
	_, err = mapInterface.Update(ctx, configMap, metav1.UpdateOptions{})
	return empty, found, err
}
