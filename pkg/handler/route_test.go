package handler

import (
	"reflect"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

func TestParseExtraRouteFromRPC_Nil(t *testing.T) {
	got := ParseExtraRouteFromRPC(nil)
	if got == nil {
		t.Fatal("ParseExtraRouteFromRPC(nil) returned nil, want non-nil empty ExtraRouteInfo")
	}
	if len(got.ExtraCIDR) != 0 {
		t.Fatalf("ExtraCIDR: want empty, got %v", got.ExtraCIDR)
	}
	if len(got.ExtraDomain) != 0 {
		t.Fatalf("ExtraDomain: want empty, got %v", got.ExtraDomain)
	}
	if got.ExtraNodeIP {
		t.Fatal("ExtraNodeIP: want false, got true")
	}
}

func TestParseExtraRouteFromRPC_Populated(t *testing.T) {
	input := &rpc.ExtraRoute{
		ExtraCIDR:   []string{"10.0.0.0/8", "192.168.1.0/24"},
		ExtraDomain: []string{"example.com", "foo.bar.io"},
		ExtraNodeIP: true,
	}

	got := ParseExtraRouteFromRPC(input)
	if got == nil {
		t.Fatal("ParseExtraRouteFromRPC returned nil")
	}
	if !reflect.DeepEqual(got.ExtraCIDR, input.ExtraCIDR) {
		t.Fatalf("ExtraCIDR: want %v, got %v", input.ExtraCIDR, got.ExtraCIDR)
	}
	if !reflect.DeepEqual(got.ExtraDomain, input.ExtraDomain) {
		t.Fatalf("ExtraDomain: want %v, got %v", input.ExtraDomain, got.ExtraDomain)
	}
	if got.ExtraNodeIP != true {
		t.Fatal("ExtraNodeIP: want true, got false")
	}
}

func TestParseExtraRouteFromRPC_EmptySlices(t *testing.T) {
	input := &rpc.ExtraRoute{
		ExtraCIDR:   []string{},
		ExtraDomain: []string{},
		ExtraNodeIP: false,
	}

	got := ParseExtraRouteFromRPC(input)
	if got == nil {
		t.Fatal("ParseExtraRouteFromRPC returned nil")
	}
	if got.ExtraCIDR == nil {
		// Empty slice from proto is fine — just verify length
		t.Log("ExtraCIDR is nil (proto may return nil for empty repeated)")
	}
	if got.ExtraNodeIP {
		t.Fatal("ExtraNodeIP: want false, got true")
	}
}

func TestExtraRouteInfo_ToRPC(t *testing.T) {
	info := ExtraRouteInfo{
		ExtraCIDR:   []string{"172.16.0.0/12"},
		ExtraDomain: []string{"svc.cluster.local"},
		ExtraNodeIP: true,
	}

	got := info.ToRPC()
	if got == nil {
		t.Fatal("ToRPC returned nil")
	}
	if !reflect.DeepEqual(got.ExtraCIDR, info.ExtraCIDR) {
		t.Fatalf("ExtraCIDR: want %v, got %v", info.ExtraCIDR, got.ExtraCIDR)
	}
	if !reflect.DeepEqual(got.ExtraDomain, info.ExtraDomain) {
		t.Fatalf("ExtraDomain: want %v, got %v", info.ExtraDomain, got.ExtraDomain)
	}
	if got.ExtraNodeIP != info.ExtraNodeIP {
		t.Fatalf("ExtraNodeIP: want %v, got %v", info.ExtraNodeIP, got.ExtraNodeIP)
	}
}

func TestExtraRouteInfo_ToRPC_Empty(t *testing.T) {
	info := ExtraRouteInfo{}

	got := info.ToRPC()
	if got == nil {
		t.Fatal("ToRPC returned nil")
	}
	if len(got.ExtraCIDR) != 0 {
		t.Fatalf("ExtraCIDR: want empty, got %v", got.ExtraCIDR)
	}
	if len(got.ExtraDomain) != 0 {
		t.Fatalf("ExtraDomain: want empty, got %v", got.ExtraDomain)
	}
	if got.ExtraNodeIP {
		t.Fatal("ExtraNodeIP: want false, got true")
	}
}

func TestExtraRouteInfo_RoundTrip(t *testing.T) {
	tests := []struct {
		name string
		info ExtraRouteInfo
	}{
		{
			name: "full",
			info: ExtraRouteInfo{
				ExtraCIDR:   []string{"10.0.0.0/8", "192.168.0.0/16", "172.16.0.0/12"},
				ExtraDomain: []string{"a.example.com", "b.example.com"},
				ExtraNodeIP: true,
			},
		},
		{
			name: "only_cidrs",
			info: ExtraRouteInfo{
				ExtraCIDR:   []string{"10.244.0.0/16"},
				ExtraDomain: nil,
				ExtraNodeIP: false,
			},
		},
		{
			name: "only_domains",
			info: ExtraRouteInfo{
				ExtraCIDR:   nil,
				ExtraDomain: []string{"my.domain.io"},
				ExtraNodeIP: false,
			},
		},
		{
			name: "only_nodeip",
			info: ExtraRouteInfo{
				ExtraCIDR:   nil,
				ExtraDomain: nil,
				ExtraNodeIP: true,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rpcMsg := tc.info.ToRPC()
			got := ParseExtraRouteFromRPC(rpcMsg)

			// nil and empty slices are equivalent for our purposes
			if len(got.ExtraCIDR) != len(tc.info.ExtraCIDR) {
				t.Fatalf("ExtraCIDR length: want %d, got %d", len(tc.info.ExtraCIDR), len(got.ExtraCIDR))
			}
			for i := range tc.info.ExtraCIDR {
				if got.ExtraCIDR[i] != tc.info.ExtraCIDR[i] {
					t.Fatalf("ExtraCIDR[%d]: want %q, got %q", i, tc.info.ExtraCIDR[i], got.ExtraCIDR[i])
				}
			}
			if len(got.ExtraDomain) != len(tc.info.ExtraDomain) {
				t.Fatalf("ExtraDomain length: want %d, got %d", len(tc.info.ExtraDomain), len(got.ExtraDomain))
			}
			for i := range tc.info.ExtraDomain {
				if got.ExtraDomain[i] != tc.info.ExtraDomain[i] {
					t.Fatalf("ExtraDomain[%d]: want %q, got %q", i, tc.info.ExtraDomain[i], got.ExtraDomain[i])
				}
			}
			if got.ExtraNodeIP != tc.info.ExtraNodeIP {
				t.Fatalf("ExtraNodeIP: want %v, got %v", tc.info.ExtraNodeIP, got.ExtraNodeIP)
			}
		})
	}
}
