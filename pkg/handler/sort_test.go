//go:build integration

package handler

import (
	"net"
	"reflect"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
)

func newConnectWithNetwork(ns string, extraCIDR []string, apiServerIPs []net.IP, extraHost []dns.Entry) *DataSession {
	ds := &DataSession{
		ManagerNamespace: ns,
		ExtraRouteInfo: ExtraRouteInfo{
			ExtraCIDR: extraCIDR,
		},
	}
	if len(apiServerIPs) > 0 || len(extraHost) > 0 {
		ds.nm = &NetworkManager{
			cfg: NetworkConfig{
				APIServerIPs: apiServerIPs,
			},
			extraHost: extraHost,
		}
	}
	return ds
}

func TestSortConnect(t *testing.T) {
	getOrder := func(connects Connects) []string {
		var order []string
		for _, connect := range connects {
			order = append(order, connect.GetManagerNamespace())
		}
		return order
	}

	tests := []struct {
		connects      Connects
		expectedOrder []string
	}{
		{
			connects: Connects{
				newConnectWithNetwork("clusterA", []string{"192.168.31.1/32"}, []net.IP{net.ParseIP("172.21.0.12")}, nil),
				newConnectWithNetwork("clusterB", []string{"10.16.31.9/32"}, []net.IP{net.ParseIP("192.168.31.1")}, nil),
			},
			expectedOrder: []string{"clusterB", "clusterA"},
		},
		{
			connects: Connects{
				newConnectWithNetwork("clusterA", []string{"192.168.31.2/32"}, []net.IP{net.ParseIP("172.21.0.12")}, []dns.Entry{{IP: "192.168.31.1"}}),
				newConnectWithNetwork("clusterB", []string{"10.16.31.9/32"}, []net.IP{net.ParseIP("192.168.31.1")}, nil),
			},
			expectedOrder: []string{"clusterB", "clusterA"},
		},
		{
			connects: Connects{
				newConnectWithNetwork("clusterA", nil, []net.IP{net.ParseIP("192.168.31.100")}, nil),
				newConnectWithNetwork("clusterB", []string{"192.168.31.2/32"}, []net.IP{net.ParseIP("172.21.0.12")}, []dns.Entry{{IP: "192.168.31.1"}}),
			},
			expectedOrder: []string{"clusterB", "clusterA"},
		},
		{
			connects: Connects{
				newConnectWithNetwork("clusterA", nil, []net.IP{net.ParseIP("192.168.31.100")}, nil),
				newConnectWithNetwork("clusterB", []string{"192.168.31.1/32"}, []net.IP{net.ParseIP("172.21.0.12")}, nil),
				newConnectWithNetwork("clusterC", []string{"10.16.31.9/32"}, []net.IP{net.ParseIP("192.168.31.1")}, nil),
			},
			expectedOrder: []string{"clusterC", "clusterB", "clusterA"},
		},
	}
	for i, test := range tests {
		order := getOrder(test.connects.Sort())
		equal := reflect.DeepEqual(order, test.expectedOrder)
		if !equal {
			t.Fatalf("Failed to sort connections round %d, expected: %v, real: %v", i+1, test.expectedOrder, order)
		}
	}
}
