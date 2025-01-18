package inject

import (
	"net/netip"
	"reflect"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func TestAddVirtualRule(t *testing.T) {
	testdatas := []struct {
		Rule    []*controlplane.Virtual
		Ports   []controlplane.ContainerPort
		Headers map[string]string
		TunIP   util.PodRouteConfig
		Uid     string
		PortMap map[int32]string

		Expect []*controlplane.Virtual
	}{
		{
			Ports: []controlplane.ContainerPort{
				{
					EnvoyListenerPort: 15006,
					ContainerPort:     9080,
				},
			},
			TunIP: util.PodRouteConfig{
				LocalTunIPv4: "127.0.0.1",
				LocalTunIPv6: netip.IPv6Loopback().String(),
			},
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
		rule := addVirtualRule(data.Rule, data.Uid, data.Ports, data.Headers, data.TunIP, nil)
		if !reflect.DeepEqual(rule, data.Expect) {
			t.FailNow()
		}
	}
}
