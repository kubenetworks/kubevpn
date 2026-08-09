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

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
	"github.com/wencaiwulue/kubevpn/v2/pkg/xds"
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
	Ports        []xds.ContainerPort
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
	return NewVirtualStore(mapInterface).AddRule(ctx, spec)
}

func addVirtualRule(ctx context.Context, v []*xds.Virtual, spec envoyRuleSpec) []*xds.Virtual {
	index := -1
	for i, virtual := range v {
		if spec.NodeID == virtual.UID && virtual.Namespace == spec.Namespace {
			index = i
			break
		}
	}

	newRule := &xds.Rule{
		Headers:      spec.Headers,
		LocalTunIPv4: spec.LocalTunIPv4,
		LocalTunIPv6: spec.LocalTunIPv6,
		OwnerID:      spec.OwnerID,
		PortMap:      spec.PortMap,
	}

	// 1) no existing Virtual for this workload — create one
	if index < 0 {
		return append(v, &xds.Virtual{
			SchemaVersion: xds.CurrentSchemaVersion,
			UID:           spec.NodeID,
			Namespace:     spec.Namespace,
			FargateMode:   spec.FargateMode,
			Ports:         spec.Ports,
			Rules:         []*xds.Rule{newRule},
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
			// Rules with headers (specific matches) must sort before the
			// header-less catch-all rule, otherwise the catch-all shadows them.
			return len(v[x].Rules[i].Headers) != 0 && len(v[x].Rules[j].Headers) == 0
		})
	}
	return v
}

func removeEnvoyConfig(ctx context.Context, mapInterface v12.ConfigMapInterface, namespace string, nodeID string, ownerID string) (empty bool, found bool, err error) {
	configMap, getErr := mapInterface.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if k8serrors.IsNotFound(getErr) {
		return true, false, nil
	}
	if getErr != nil {
		return false, false, getErr
	}
	if _, ok := configMap.Data[config.KeyEnvoy]; !ok {
		return false, false, fmt.Errorf("cannot find value for key: envoy-config.yaml: %w", config.ErrCleanupFailed)
	}
	return NewVirtualStore(mapInterface).RemoveRule(ctx, namespace, nodeID, ownerID)
}
