package util

import (
	"encoding/binary"
	"net"
	"strings"
	"testing"
)

func TestParseIPFast_IPv4(t *testing.T) {
	// Minimal IPv4 packet: version=4, IHL=5, src=10.0.0.1, dst=192.168.1.1, protocol=6(TCP)
	pkt := make([]byte, 20)
	pkt[0] = 0x45
	pkt[9] = 6
	copy(pkt[12:16], net.IPv4(10, 0, 0, 1).To4())
	copy(pkt[16:20], net.IPv4(192, 168, 1, 1).To4())

	src, dst, proto, err := ParseIPFast(pkt)
	if err != nil {
		t.Fatalf("ParseIPFast: %v", err)
	}
	if !src.Equal(net.IPv4(10, 0, 0, 1).To4()) {
		t.Fatalf("src: want 10.0.0.1, got %s", src)
	}
	if !dst.Equal(net.IPv4(192, 168, 1, 1).To4()) {
		t.Fatalf("dst: want 192.168.1.1, got %s", dst)
	}
	if proto != 6 {
		t.Fatalf("protocol: want 6, got %d", proto)
	}
}

func TestParseIPFast_IPv6(t *testing.T) {
	// Minimal IPv6 packet: version=6, next header=17(UDP)
	pkt := make([]byte, 40)
	pkt[0] = 0x60
	pkt[6] = 17 // next header = UDP
	// src: 2001:db8::1
	src6 := net.ParseIP("2001:db8::1")
	copy(pkt[8:24], src6.To16())
	// dst: 2001:db8::2
	dst6 := net.ParseIP("2001:db8::2")
	copy(pkt[24:40], dst6.To16())

	src, dst, proto, err := ParseIPFast(pkt)
	if err != nil {
		t.Fatalf("ParseIPFast IPv6: %v", err)
	}
	if !src.Equal(src6) {
		t.Fatalf("src: want %s, got %s", src6, src)
	}
	if !dst.Equal(dst6) {
		t.Fatalf("dst: want %s, got %s", dst6, dst)
	}
	if proto != 17 {
		t.Fatalf("protocol: want 17, got %d", proto)
	}
}

func TestParseIPFast_TooShort(t *testing.T) {
	_, _, _, err := ParseIPFast([]byte{0x45, 0x00})
	if err == nil {
		t.Fatal("expected error for short packet")
	}
}

func TestParseIPFast_InvalidVersion(t *testing.T) {
	pkt := make([]byte, 20)
	pkt[0] = 0x30 // version 3
	_, _, _, err := ParseIPFast(pkt)
	if err == nil {
		t.Fatal("expected error for invalid version")
	}
}

func TestIsIPv4(t *testing.T) {
	cases := []struct {
		name   string
		header byte
		want   bool
	}{
		{"v4", 0x45, true},
		{"v4 with options", 0x4F, true},
		{"v6", 0x60, false},
		{"garbage", 0x30, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pkt := []byte{c.header}
			if got := IsIPv4(pkt); got != c.want {
				t.Fatalf("IsIPv4(0x%02x): want %v, got %v", c.header, c.want, got)
			}
		})
	}
}

func TestIsIPv6(t *testing.T) {
	cases := []struct {
		name   string
		header byte
		want   bool
	}{
		{"v6", 0x60, true},
		{"v6 traffic class", 0x6F, true},
		{"v4", 0x45, false},
		{"garbage", 0x30, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pkt := []byte{c.header}
			if got := IsIPv6(pkt); got != c.want {
				t.Fatalf("IsIPv6(0x%02x): want %v, got %v", c.header, c.want, got)
			}
		})
	}
}

func TestIsValidCIDR(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"10.0.0.1/24", true},
		{"192.168.0.0/16", true},
		{"2001:db8::/32", true},
		{"198.18.0.1/32", true},
		{"invalid", false},
		{"10.0.0.1", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			if got := IsValidCIDR(c.input); got != c.want {
				t.Fatalf("IsValidCIDR(%q): want %v, got %v", c.input, c.want, got)
			}
		})
	}
}

func TestParseDirMapping(t *testing.T) {
	// Use /tmp which always exists
	cases := []struct {
		input       string
		wantLocal   string
		wantRemote  string
		wantErr     bool
	}{
		{"/tmp:/remote", "/tmp", "/remote", false},
		{"no-separator", "", "", true},
		{"", "", "", true},
		{"/nonexistent-path-xyz:/app", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			local, remote, err := ParseDirMapping(c.input)
			if (err != nil) != c.wantErr {
				t.Fatalf("ParseDirMapping(%q): err=%v, wantErr=%v", c.input, err, c.wantErr)
			}
			if !c.wantErr {
				if local != c.wantLocal {
					t.Fatalf("local: want %q, got %q", c.wantLocal, local)
				}
				if remote != c.wantRemote {
					t.Fatalf("remote: want %q, got %q", c.wantRemote, remote)
				}
			}
		})
	}
}

func TestSafeWrite(t *testing.T) {
	ch := make(chan int, 1)
	if !SafeWrite(ch, 42) {
		t.Fatal("SafeWrite to buffered chan should return true")
	}
	val := <-ch
	if val != 42 {
		t.Fatalf("expected 42, got %d", val)
	}
}

func TestSafeWrite_Full(t *testing.T) {
	ch := make(chan int, 1)
	ch <- 1 // fill buffer
	if SafeWrite(ch, 2) {
		t.Fatal("SafeWrite to full chan should return false")
	}
}

func TestSafeClose(t *testing.T) {
	ch := make(chan struct{}, 1)
	SafeClose(ch)
	// Should not panic on second close
	SafeClose(ch)
}

func TestGenICMPPacket(t *testing.T) {
	src := net.IPv4(198, 18, 0, 1)
	dst := net.IPv4(198, 18, 0, 0)
	pkt, err := GenICMPPacket(src, dst)
	if err != nil {
		t.Fatalf("GenICMPPacket: %v", err)
	}
	if len(pkt) < 28 {
		t.Fatalf("packet too short: %d bytes", len(pkt))
	}
	// Verify it's IPv4
	if pkt[0]>>4 != 4 {
		t.Fatalf("not IPv4: version=%d", pkt[0]>>4)
	}
	// Verify protocol is ICMP (1)
	if pkt[9] != 1 {
		t.Fatalf("protocol: want 1 (ICMP), got %d", pkt[9])
	}
	// Verify total length is consistent
	totalLen := binary.BigEndian.Uint16(pkt[2:4])
	if int(totalLen) != len(pkt) {
		t.Fatalf("total length: header says %d, actual %d", totalLen, len(pkt))
	}
}

func TestGenICMPPacketIPv6(t *testing.T) {
	src := net.ParseIP("2001:2::1")
	dst := net.ParseIP("2001:2::0")
	pkt, err := GenICMPPacketIPv6(src, dst)
	if err != nil {
		t.Fatalf("GenICMPPacketIPv6: %v", err)
	}
	if len(pkt) < 44 {
		t.Fatalf("packet too short: %d bytes", len(pkt))
	}
	// Verify it's IPv6
	if pkt[0]>>4 != 6 {
		t.Fatalf("not IPv6: version=%d", pkt[0]>>4)
	}
}

func TestParseIP_IPv4(t *testing.T) {
	srcIP := net.IPv4(10, 0, 0, 1)
	dstIP := net.IPv4(192, 168, 1, 1)

	// Generate a real IPv4 ICMP packet to parse.
	pkt, err := GenICMPPacket(srcIP, dstIP)
	if err != nil {
		t.Fatalf("GenICMPPacket: %v", err)
	}

	src, dst, proto, err := ParseIP(pkt)
	if err != nil {
		t.Fatalf("ParseIP IPv4: %v", err)
	}
	if !src.Equal(srcIP.To4()) {
		t.Fatalf("src: want %s, got %s", srcIP, src)
	}
	if !dst.Equal(dstIP.To4()) {
		t.Fatalf("dst: want %s, got %s", dstIP, dst)
	}
	// ICMP protocol number is 1
	if proto != 1 {
		t.Fatalf("protocol: want 1 (ICMP), got %d", proto)
	}
}

func TestParseIP_IPv6(t *testing.T) {
	srcIP := net.ParseIP("2001:db8::1")
	dstIP := net.ParseIP("2001:db8::2")

	// Generate a real IPv6 ICMPv6 packet to parse.
	pkt, err := GenICMPPacketIPv6(srcIP, dstIP)
	if err != nil {
		t.Fatalf("GenICMPPacketIPv6: %v", err)
	}

	src, dst, proto, err := ParseIP(pkt)
	if err != nil {
		t.Fatalf("ParseIP IPv6: %v", err)
	}
	if !src.Equal(srcIP) {
		t.Fatalf("src: want %s, got %s", srcIP, src)
	}
	if !dst.Equal(dstIP) {
		t.Fatalf("dst: want %s, got %s", dstIP, dst)
	}
	// ICMPv6 protocol number is 58
	if proto != 58 {
		t.Fatalf("protocol: want 58 (ICMPv6), got %d", proto)
	}
}

func TestGetLocalIPNet(t *testing.T) {
	ipNet, err := GetLocalIPNet()
	if err != nil {
		t.Fatalf("GetLocalIPNet: %v", err)
	}
	if ipNet == nil {
		t.Fatal("GetLocalIPNet returned nil")
	}
	if ipNet.IP == nil {
		t.Fatal("GetLocalIPNet returned nil IP")
	}
	// The returned IP must be an IPv4 address (from DockerCIDR).
	if ipNet.IP.To4() == nil {
		t.Fatalf("expected IPv4, got %s", ipNet.IP)
	}
	// The IP must not be the loopback address.
	if ipNet.IP.IsLoopback() {
		t.Fatalf("expected non-loopback IP, got %s", ipNet.IP)
	}
}

func TestConvertUIDToWorkload(t *testing.T) {
	cases := []struct{ uid, want string }{
		{"deployments.apps.productpage", "deployments.apps/productpage"},
		{"statefulsets.apps.redis", "statefulsets.apps/redis"},
		{"pods.my-pod", "pods/my-pod"},
	}
	for _, c := range cases {
		if got := ConvertUIDToWorkload(c.uid); got != c.want {
			t.Errorf("ConvertUIDToWorkload(%q) = %q, want %q", c.uid, got, c.want)
		}
	}
}

func TestConvertWorkloadToUID(t *testing.T) {
	cases := []struct{ workload, want string }{
		{"deployments.apps/productpage", "deployments.apps.productpage"},
		{"statefulsets.apps/redis", "statefulsets.apps.redis"},
		{"pods/my-pod", "pods.my-pod"},
	}
	for _, c := range cases {
		if got := ConvertWorkloadToUID(c.workload); got != c.want {
			t.Errorf("ConvertWorkloadToUID(%q) = %q, want %q", c.workload, got, c.want)
		}
	}
}

func TestConvertUIDRoundTrip(t *testing.T) {
	workload := "deployments.apps/productpage"
	uid := ConvertWorkloadToUID(workload)
	back := ConvertUIDToWorkload(uid)
	if back != workload {
		t.Errorf("round-trip failed: %q -> %q -> %q", workload, uid, back)
	}
}

func TestMerge(t *testing.T) {
	t.Run("merges two maps", func(t *testing.T) {
		from := map[string]int{"a": 1, "b": 2}
		to := map[string]int{"c": 3}
		result := Merge(from, to)
		if result["a"] != 1 || result["b"] != 2 || result["c"] != 3 {
			t.Errorf("unexpected merge result: %v", result)
		}
	})
	t.Run("to overwrites from on conflict", func(t *testing.T) {
		from := map[string]int{"a": 10}
		to := map[string]int{"a": 1}
		result := Merge(from, to)
		if result["a"] != 1 {
			t.Errorf("expected to to overwrite from: got %d", result["a"])
		}
	})
	t.Run("nil inputs", func(t *testing.T) {
		result := Merge[string, int](nil, nil)
		if len(result) != 0 {
			t.Errorf("expected empty map, got %v", result)
		}
	})
}

func TestIf(t *testing.T) {
	if got := If(true, "yes", "no"); got != "yes" {
		t.Errorf("If(true) = %q, want yes", got)
	}
	if got := If(false, "yes", "no"); got != "no" {
		t.Errorf("If(false) = %q, want no", got)
	}
	if got := If(true, 42, 0); got != 42 {
		t.Errorf("If[int](true) = %d, want 42", got)
	}
}

func TestFormatBanner_ContainsSlogan(t *testing.T) {
	result := FormatBanner("test banner")
	if result == "" {
		t.Error("FormatBanner should return non-empty string")
	}
	if !strings.Contains(result, "test banner") {
		t.Errorf("FormatBanner should contain the slogan, got: %s", result)
	}
}
