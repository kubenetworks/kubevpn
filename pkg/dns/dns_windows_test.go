//go:build windows

package dns

import (
	"net/netip"
	"reflect"
	"testing"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

func TestFilterServersForFamily(t *testing.T) {
	v4a := netip.MustParseAddr("10.96.0.10")
	v4b := netip.MustParseAddr("172.16.0.1")
	v6a := netip.MustParseAddr("fd00::10")

	servers := []netip.Addr{v4a, v6a, v4b}

	if got := filterServersForFamily(windows.AF_INET, servers); !reflect.DeepEqual(got, []netip.Addr{v4a, v4b}) {
		t.Fatalf("AF_INET filter = %v, want [%v %v]", got, v4a, v4b)
	}
	if got := filterServersForFamily(windows.AF_INET6, servers); !reflect.DeepEqual(got, []netip.Addr{v6a}) {
		t.Fatalf("AF_INET6 filter = %v, want [%v]", got, v6a)
	}
	if got := filterServersForFamily(windows.AF_INET6, []netip.Addr{v4a, v4b}); got != nil {
		t.Fatalf("AF_INET6 filter over v4-only = %v, want nil", got)
	}
}

func TestBuildNetshDNSCmds(t *testing.T) {
	v4a := netip.MustParseAddr("10.96.0.10")
	v4b := netip.MustParseAddr("172.16.0.1")
	v6a := netip.MustParseAddr("fd00::10")
	const idx = 42

	tests := []struct {
		name   string
		family winipcfg.AddressFamily
		in     []netip.Addr
		want   []string
	}{
		{
			name:   "ipv4 two servers",
			family: windows.AF_INET,
			in:     []netip.Addr{v4a, v6a, v4b},
			want: []string{
				"interface ipv4 set dnsservers name=42 source=static address=none validate=no register=both",
				"interface ipv4 add dnsservers name=42 address=10.96.0.10 validate=no",
				"interface ipv4 add dnsservers name=42 address=172.16.0.1 validate=no",
			},
		},
		{
			name:   "ipv6 one server",
			family: windows.AF_INET6,
			in:     []netip.Addr{v4a, v6a},
			want: []string{
				"interface ipv6 set dnsservers name=42 source=static address=none validate=no register=both",
				"interface ipv6 add dnsservers name=42 address=fd00::10 validate=no",
			},
		},
		{
			name:   "ipv6 with no v6 servers yields nil",
			family: windows.AF_INET6,
			in:     []netip.Addr{v4a, v4b},
			want:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildNetshDNSCmds(idx, tt.family, tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("buildNetshDNSCmds = %#v, want %#v", got, tt.want)
			}
		})
	}
}
