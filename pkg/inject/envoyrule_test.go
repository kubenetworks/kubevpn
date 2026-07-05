package inject

import (
	"net/netip"
	"reflect"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
)

func TestAddVirtualRule(t *testing.T) {
	testdatas := []struct {
		Rule      []*controlplane.Virtual
		Ports     []controlplane.ContainerPort
		Headers   map[string]string
		LocalTunIPv4 string
		LocalTunIPv6 string
		UID       string
		Namespace string
		PortMap   map[int32]string

		Expect []*controlplane.Virtual
	}{
		{
			Ports: []controlplane.ContainerPort{
				{
					EnvoyListenerPort: 15006,
					ContainerPort:     9080,
				},
			},
			LocalTunIPv4: "127.0.0.1",
			LocalTunIPv6: netip.IPv6Loopback().String(),
			UID: "deployments.authors",
			Expect: []*controlplane.Virtual{
				{
					SchemaVersion: controlplane.CurrentSchemaVersion,
					UID:           "deployments.authors",
					Ports: []controlplane.ContainerPort{
						{
							EnvoyListenerPort: 15006,
							ContainerPort:     9080,
						},
					},
					Rules: []*controlplane.Rule{{
						Headers:      nil,
						LocalTunIPv4: "127.0.0.1",
						LocalTunIPv6: netip.IPv6Loopback().String(),
						OwnerID:      "test-owner",
						PortMap:      nil,
					}},
				},
			},
		},
	}
	for _, data := range testdatas {
		rule := addVirtualRule(data.Rule, envoyRuleSpec{Namespace: data.Namespace, NodeID: data.UID, Ports: data.Ports, Headers: data.Headers, LocalTunIPv4: data.LocalTunIPv4, LocalTunIPv6: data.LocalTunIPv6, OwnerID: "test-owner"})
		if !reflect.DeepEqual(rule, data.Expect) {
			t.FailNow()
		}
	}
}
