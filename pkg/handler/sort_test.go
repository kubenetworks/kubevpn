//go:build integration

package handler

import (
	"net"
	"reflect"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
)

func TestSortConnect(t *testing.T) {
	getOrder := func(connects []*ConnectOptions) []string {
		var order []string
		for _, connect := range connects {
			order = append(order, connect.ManagerNamespace)
		}
		return order
	}

	tests := []struct {
		connects      Connects
		expectedOrder []string
	}{
		{
			connects: []*ConnectOptions{
				{
					ManagerNamespace: "clusterA",
					ExtraRouteInfo: ExtraRouteInfo{
						ExtraCIDR: []string{"192.168.31.1/32"},
					},
					apiServerIPs: []net.IP{net.ParseIP("172.21.0.12")},
					extraHost:    nil,
				},
				{
					ManagerNamespace: "clusterB",
					ExtraRouteInfo: ExtraRouteInfo{
						ExtraCIDR: []string{"10.16.31.9/32"},
					},
					apiServerIPs: []net.IP{net.ParseIP("192.168.31.1")},
					extraHost:    nil,
				},
			},
			expectedOrder: []string{"clusterB", "clusterA"},
		},
		{
			connects: []*ConnectOptions{
				{
					ManagerNamespace: "clusterA",
					ExtraRouteInfo: ExtraRouteInfo{
						ExtraCIDR: []string{"192.168.31.2/32"},
					},
					apiServerIPs: []net.IP{net.ParseIP("172.21.0.12")},
					extraHost: []dns.Entry{{
						IP: "192.168.31.1",
					}},
				},
				{
					ManagerNamespace: "clusterB",
					ExtraRouteInfo: ExtraRouteInfo{
						ExtraCIDR: []string{"10.16.31.9/32"},
					},
					apiServerIPs: []net.IP{net.ParseIP("192.168.31.1")},
					extraHost:    nil,
				},
			},
			expectedOrder: []string{"clusterB", "clusterA"},
		},
		{
			connects: []*ConnectOptions{
				{
					ManagerNamespace: "clusterA",
					ExtraRouteInfo:   ExtraRouteInfo{},
					apiServerIPs:     []net.IP{net.ParseIP("192.168.31.100")},
					extraHost:        nil,
				},
				{
					ManagerNamespace: "clusterB",
					ExtraRouteInfo: ExtraRouteInfo{
						ExtraCIDR: []string{"192.168.31.2/32"},
					},
					apiServerIPs: []net.IP{net.ParseIP("172.21.0.12")},
					extraHost: []dns.Entry{{
						IP: "192.168.31.1",
					}},
				},
			},
			expectedOrder: []string{"clusterB", "clusterA"},
		},
		{
			connects: []*ConnectOptions{
				{
					ManagerNamespace: "clusterA",
					ExtraRouteInfo:   ExtraRouteInfo{},
					apiServerIPs:     []net.IP{net.ParseIP("192.168.31.100")},
					extraHost:        nil,
				},
				{
					ManagerNamespace: "clusterB",
					ExtraRouteInfo: ExtraRouteInfo{
						ExtraCIDR: []string{"192.168.31.1/32"},
					},
					apiServerIPs: []net.IP{net.ParseIP("172.21.0.12")},
					extraHost:    nil,
				},
				{
					ManagerNamespace: "clusterC",
					ExtraRouteInfo: ExtraRouteInfo{
						ExtraCIDR: []string{"10.16.31.9/32"},
					},
					apiServerIPs: []net.IP{net.ParseIP("192.168.31.1")},
					extraHost:    nil,
				},
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
