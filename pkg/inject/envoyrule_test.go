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
		Uid       string
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
			Uid: "deployments.authors",
			Expect: []*controlplane.Virtual{
				{
					Uid: "deployments.authors",
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
		rule := addVirtualRule(data.Rule, data.Namespace, data.Uid, data.Ports, data.Headers, data.LocalTunIPv4, data.LocalTunIPv6, nil)
		if !reflect.DeepEqual(rule, data.Expect) {
			t.FailNow()
		}
	}
}
