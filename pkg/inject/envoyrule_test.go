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
					UID: "deployments.authors",
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
						PortMap:      nil,
					}},
				},
			},
		},
	}
	for _, data := range testdatas {
		rule := addVirtualRule(data.Rule, data.Namespace, data.UID, data.Ports, data.Headers, data.LocalTunIPv4, data.LocalTunIPv6, nil, false)
		if !reflect.DeepEqual(rule, data.Expect) {
			t.FailNow()
		}
	}
}
