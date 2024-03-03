package handler

import (
	"net"
	"reflect"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
)

func TestSortConnect(t *testing.T) {
	tests := []struct {
		connects      Connects
		expectedOrder []string
		howToGetOrder func(connects []*ConnectOptions) []string
	}{
		{
			connects: []*ConnectOptions{
				{
					Namespace: "clusterA",
					ExtraRouteInfo: ExtraRouteInfo{
						ExtraCIDR: []string{"192.168.31.1/32"},
					},
					apiServerIPs: []net.IP{net.ParseIP("172.21.0.12")},
					extraHost:    nil,
				},
				{
					Namespace: "clusterB",
					ExtraRouteInfo: ExtraRouteInfo{
						ExtraCIDR: []string{"10.16.31.9/32"},
					},
					apiServerIPs: []net.IP{net.ParseIP("192.168.31.1")},
					extraHost:    nil,
				},
			},
			expectedOrder: []string{"clusterB", "clusterA"},
			howToGetOrder: func(connects []*ConnectOptions) []string {
				var order []string
				for _, connect := range connects {
					order = append(order, connect.Namespace)
				}
				return order
			},
		},
		{
			connects: []*ConnectOptions{
				{
					Namespace: "clusterA",
					ExtraRouteInfo: ExtraRouteInfo{
						ExtraCIDR: []string{"192.168.31.2/32"},
					},
					apiServerIPs: []net.IP{net.ParseIP("172.21.0.12")},
					extraHost: []dns.Entry{{
						IP: "192.168.31.1",
					}},
				},
				{
					Namespace: "clusterB",
					ExtraRouteInfo: ExtraRouteInfo{
						ExtraCIDR: []string{"10.16.31.9/32"},
					},
					apiServerIPs: []net.IP{net.ParseIP("192.168.31.1")},
					extraHost:    nil,
				},
			},
			expectedOrder: []string{"clusterB", "clusterA"},
			howToGetOrder: func(connects []*ConnectOptions) []string {
				var order []string
				for _, connect := range connects {
					order = append(order, connect.Namespace)
				}
				return order
			},
		},
		{
			connects: []*ConnectOptions{
				{
					Namespace:      "clusterA",
					ExtraRouteInfo: ExtraRouteInfo{},
					apiServerIPs:   []net.IP{net.ParseIP("192.168.31.100")},
					extraHost:      nil,
				},
				{
					Namespace: "clusterB",
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
			howToGetOrder: func(connects []*ConnectOptions) []string {
				var order []string
				for _, connect := range connects {
					order = append(order, connect.Namespace)
				}
				return order
			},
		},
		{
			connects: []*ConnectOptions{
				{
					Namespace:      "clusterA",
					ExtraRouteInfo: ExtraRouteInfo{},
					apiServerIPs:   []net.IP{net.ParseIP("192.168.31.100")},
					extraHost:      nil,
				},
				{
					Namespace: "clusterB",
					ExtraRouteInfo: ExtraRouteInfo{
						ExtraCIDR: []string{"192.168.31.1/32"},
					},
					apiServerIPs: []net.IP{net.ParseIP("172.21.0.12")},
					extraHost:    nil,
				},
				{
					Namespace: "clusterC",
					ExtraRouteInfo: ExtraRouteInfo{
						ExtraCIDR: []string{"10.16.31.9/32"},
					},
					apiServerIPs: []net.IP{net.ParseIP("192.168.31.1")},
					extraHost:    nil,
				},
			},
			expectedOrder: []string{"clusterC", "clusterB", "clusterA"},
			howToGetOrder: func(connects []*ConnectOptions) []string {
				var order []string
				for _, connect := range connects {
					order = append(order, connect.Namespace)
				}
				return order
			},
		},
	}
	for i, test := range tests {
		order := test.howToGetOrder(test.connects.Sort())
		equal := reflect.DeepEqual(order, test.expectedOrder)
		if !equal {
			t.Fatalf("failed to sort conntions round %d, expected: %v, real: %v", i+1, test.expectedOrder, order)
		}
	}
}
