package core

import (
	"net"
	"testing"
)

func TestIPHash_Deterministic(t *testing.T) {
	ip := net.ParseIP("10.0.0.1")
	slots := 4
	expected := ipHash(ip, slots)
	for i := 0; i < 100; i++ {
		if got := ipHash(ip, slots); got != expected {
			t.Fatalf("ipHash not deterministic: call %d returned %d, expected %d", i, got, expected)
		}
	}
}

func TestIPHash_DifferentIPs(t *testing.T) {
	slots := 64
	results := make(map[int]bool)
	ips := []string{
		"10.0.0.1",
		"10.0.0.2",
		"192.168.1.1",
		"172.16.0.1",
		"8.8.8.8",
		"1.1.1.1",
		"255.255.255.255",
		"0.0.0.0",
	}
	for _, s := range ips {
		ip := net.ParseIP(s).To4()
		results[ipHash(ip, slots)] = true
	}
	if len(results) < 2 {
		t.Fatalf("expected different IPs to produce at least 2 distinct slots, got %d", len(results))
	}
}

func TestIPHash_InRange(t *testing.T) {
	ips := []net.IP{
		net.ParseIP("10.0.0.1").To4(),
		net.ParseIP("192.168.1.100").To4(),
		net.ParseIP("255.255.255.255").To4(),
		net.ParseIP("0.0.0.0").To4(),
		net.ParseIP("::1"),
		net.ParseIP("fe80::1"),
		net.ParseIP("2001:db8::1"),
	}
	slotCounts := []int{1, 2, 3, 4, 7, 16, 100, 256}
	for _, ip := range ips {
		for _, slots := range slotCounts {
			got := ipHash(ip, slots)
			if got < 0 || got >= slots {
				t.Errorf("ipHash(%s, %d) = %d, want [0, %d)", ip, slots, got, slots)
			}
		}
	}
}

func TestIPHash_IPv4AndIPv6(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		to4  bool
	}{
		{"IPv4 loopback", "127.0.0.1", true},
		{"IPv4 private", "10.244.0.5", true},
		{"IPv6 loopback", "::1", false},
		{"IPv6 link-local", "fe80::1", false},
		{"IPv6 global", "2001:db8::ff00:42:8329", false},
	}
	slots := 8
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if tt.to4 {
				ip = ip.To4()
			}
			got := ipHash(ip, slots)
			if got < 0 || got >= slots {
				t.Errorf("ipHash(%s, %d) = %d, want [0, %d)", ip, slots, got, slots)
			}
			// Verify determinism for this specific IP
			if got2 := ipHash(ip, slots); got2 != got {
				t.Errorf("ipHash(%s, %d) not deterministic: %d then %d", ip, slots, got, got2)
			}
		})
	}
}

func TestIPHash_SingleSlot(t *testing.T) {
	ips := []string{
		"0.0.0.0",
		"10.0.0.1",
		"192.168.1.1",
		"255.255.255.255",
		"::1",
		"fe80::1",
		"2001:db8::1",
	}
	for _, s := range ips {
		ip := net.ParseIP(s)
		if ip4 := ip.To4(); ip4 != nil {
			ip = ip4
		}
		if got := ipHash(ip, 1); got != 0 {
			t.Errorf("ipHash(%s, 1) = %d, want 0", s, got)
		}
	}
}
