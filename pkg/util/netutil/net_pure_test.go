package netutil

import (
	"encoding/binary"
	"net"
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
	// Regression: a valid ICMPv6 echo request needs the 4-byte Identifier/SeqNumber body, so
	// the packet must be at least IPv6(40) + ICMPv6 echo minimum(8) = 48 bytes. It was
	// previously only 44 (the 4-byte ICMPv6 base header with no echo body), which gvisor and
	// real hosts treat as malformed and never answer — breaking the IPv6 heartbeat/registration.
	if len(pkt) < 48 {
		t.Fatalf("packet too short: %d bytes (missing ICMPv6 echo id/seq body?)", len(pkt))
	}
	// Verify it's IPv6
	if pkt[0]>>4 != 6 {
		t.Fatalf("not IPv6: version=%d", pkt[0]>>4)
	}
	// ICMPv6 header starts at offset 40; type 128 = Echo Request.
	if pkt[40] != 128 {
		t.Fatalf("ICMPv6 type = %d, want EchoRequest(128)", pkt[40])
	}
	// Regression: the ICMPv6 checksum (IPv6 header is 40 bytes; checksum at offset 42-43)
	// must be computed — it was previously left zero, which receivers reject.
	if csum := binary.BigEndian.Uint16(pkt[42:44]); csum == 0 {
		t.Fatal("ICMPv6 checksum is zero (regression: must be computed over the pseudo-header)")
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
