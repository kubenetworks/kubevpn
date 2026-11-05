package discovery

import (
	"net"
	"testing"
)

func TestPodIPToPrefix(t *testing.T) {
	cases := []struct {
		ip   string
		want string
	}{
		{"10.244.1.5", "10.244.1.0/24"},
		{"10.244.1.250", "10.244.1.0/24"},
		{"10.244.2.3", "10.244.2.0/24"},
		{"172.16.5.9", "172.16.5.0/24"},
		{"fd00:10:244::5", "fd00:10:244::/64"},
	}
	for _, c := range cases {
		got, ok := podIPToPrefix(net.ParseIP(c.ip))
		if !ok || got != c.want {
			t.Errorf("podIPToPrefix(%s) = %q,%v; want %q", c.ip, got, ok, c.want)
		}
	}
	if _, ok := podIPToPrefix(nil); ok {
		t.Errorf("podIPToPrefix(nil) should be !ok")
	}
	// Aggressive aggregation must never widen beyond /24 (v4) / /64 (v6).
	if got, _ := podIPToPrefix(net.ParseIP("10.244.1.5")); got == "10.244.0.0/16" || got == "10.0.0.0/8" {
		t.Errorf("prefix over-aggregated: %s", got)
	}
}
