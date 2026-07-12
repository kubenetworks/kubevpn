package inject

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
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
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
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

// envoyRuleSpec captures the parameters for adding a proxy rule to the Envoy config.
// Avoids passing 10+ individual arguments through addEnvoyConfig → addVirtualRule.
type envoyRuleSpec struct {
	Namespace    string
	NodeID       string
	LocalTunIPv4 string
	LocalTunIPv6 string
	Headers      map[string]string
	Ports        []controlplane.ContainerPort
	PortMap      map[int32]string
	FargateMode  bool
	OwnerID      string
}

// validate checks that all required fields are populated.
func (s *envoyRuleSpec) validate() error {
	if s.Namespace == "" {
		return errors.New("envoyRuleSpec: namespace must not be empty")
	}
	if s.NodeID == "" {
		return errors.New("envoyRuleSpec: nodeID must not be empty")
	}
	if s.LocalTunIPv4 == "" {
		return errors.New("envoyRuleSpec: localTunIPv4 must not be empty")
	}
	if s.OwnerID == "" {
		return errors.New("envoyRuleSpec: ownerID must not be empty")
	}
	return nil
}

func addEnvoyConfig(ctx context.Context, mapInterface v12.ConfigMapInterface, spec envoyRuleSpec) error {
	if err := spec.validate(); err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		configMap, err := mapInterface.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if err != nil {
			return err
		}
		v := make([]*controlplane.Virtual, 0)
		if str, ok := configMap.Data[config.KeyEnvoy]; ok {
			if err = yaml.Unmarshal([]byte(str), &v); err != nil {
				return err
			}
		}

		v = addVirtualRule(ctx, v, spec)
		marshal, err := yaml.Marshal(v)
		if err != nil {
			return err
		}
		configMap.Data[config.KeyEnvoy] = string(marshal)
		_, err = mapInterface.Update(ctx, configMap, metav1.UpdateOptions{})
		return err
	})
}

func addVirtualRule(ctx context.Context, v []*controlplane.Virtual, spec envoyRuleSpec) []*controlplane.Virtual {
	index := -1
	for i, virtual := range v {
		if spec.NodeID == virtual.UID && virtual.Namespace == spec.Namespace {
			index = i
			break
		}
	}

	newRule := &controlplane.Rule{
		Headers:      spec.Headers,
		LocalTunIPv4: spec.LocalTunIPv4,
		LocalTunIPv6: spec.LocalTunIPv6,
		OwnerID:      spec.OwnerID,
		PortMap:      spec.PortMap,
	}

	// 1) no existing Virtual for this workload — create one
	if index < 0 {
		return append(v, &controlplane.Virtual{
			SchemaVersion: controlplane.CurrentSchemaVersion,
			UID:           spec.NodeID,
			Namespace:     spec.Namespace,
			FargateMode:   spec.FargateMode,
			Ports:         spec.Ports,
			Rules:         []*controlplane.Rule{newRule},
		})
	}

	// 2) same owner updating their own rule (matched by OwnerID, non-fargate)
	if !v[index].IsFargateMode() {
		for j, rule := range v[index].Rules {
			if rule.OwnerID == spec.OwnerID {
				v[index].Rules[j].Headers = util.Merge[string, string](v[index].Rules[j].Headers, spec.Headers)
				v[index].Rules[j].PortMap = util.Merge[int32, string](v[index].Rules[j].PortMap, spec.PortMap)
				v[index].Rules[j].LocalTunIPv4 = spec.LocalTunIPv4
				v[index].Rules[j].LocalTunIPv6 = spec.LocalTunIPv6
				return v
			}
		}
	}

	// 3) different user taking over a rule with matching headers
	for j, rule := range v[index].Rules {
		if maps.Equal(rule.Headers, spec.Headers) {
			plog.G(ctx).Warnf("[Envoy] Header takeover: owner %s is replacing owner %s for workload %s with headers %v",
				spec.OwnerID, rule.OwnerID, spec.NodeID, spec.Headers)
			v[index].Rules[j].LocalTunIPv6 = spec.LocalTunIPv6
			v[index].Rules[j].LocalTunIPv4 = spec.LocalTunIPv4
			v[index].Rules[j].OwnerID = spec.OwnerID
			v[index].Rules[j].PortMap = spec.PortMap
			return v
		}
	}

	// 4) new user with different headers — append
	v[index].Rules = append(v[index].Rules, newRule)
	if v[index].Ports == nil {
		v[index].Ports = spec.Ports
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

func removeEnvoyConfig(ctx context.Context, mapInterface v12.ConfigMapInterface, namespace string, nodeID string, ownerID string) (empty bool, found bool, err error) {
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		// Reset per-attempt state so retries start clean.
		empty = false
		found = false

		configMap, getErr := mapInterface.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if k8serrors.IsNotFound(getErr) {
			empty = true
			return nil
		}
		if getErr != nil {
			return getErr
		}
		str, ok := configMap.Data[config.KeyEnvoy]
		if !ok {
			return fmt.Errorf("cannot find value for key: envoy-config.yaml")
		}
		var v []*controlplane.Virtual
		if unmarshalErr := yaml.Unmarshal([]byte(str), &v); unmarshalErr != nil {
			return unmarshalErr
		}
		for _, virtual := range v {
			if nodeID == virtual.UID && namespace == virtual.Namespace {
				for i := 0; i < len(virtual.Rules); i++ {
					if ownerID == virtual.Rules[i].OwnerID {
						found = true
						virtual.Rules = append(virtual.Rules[:i], virtual.Rules[i+1:]...)
						i--
					}
				}
			}
		}
		if !found {
			return nil
		}

		// remove default
		for i := 0; i < len(v); i++ {
			if nodeID == v[i].UID && namespace == v[i].Namespace && len(v[i].Rules) == 0 {
				v = append(v[:i], v[i+1:]...)
				i--
				empty = true
			}
		}
		b, marshalErr := yaml.Marshal(v)
		if marshalErr != nil {
			return marshalErr
		}
		configMap.Data[config.KeyEnvoy] = string(b)
		_, updateErr := mapInterface.Update(ctx, configMap, metav1.UpdateOptions{})
		return updateErr
	})
	return empty, found, err
}
