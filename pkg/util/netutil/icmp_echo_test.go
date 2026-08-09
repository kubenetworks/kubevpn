package netutil

import (
	"net"
	"testing"
)

// buildICMPv4 builds a minimal IPv4 + ICMP packet with the given source, dest and ICMP type.
func buildICMPv4(src, dst net.IP, icmpType byte) []byte {
	pkt := make([]byte, 28) // 20 IPv4 header + 8 ICMP
	pkt[0] = 0x45           // version 4, IHL 5
	pkt[9] = 1              // protocol ICMP
	copy(pkt[12:16], src.To4())
	copy(pkt[16:20], dst.To4())
	pkt[20] = icmpType // ICMP type
	return pkt
}

// buildICMPv6 builds a minimal IPv6 + ICMPv6 packet with the given source, dest and ICMP type.
func buildICMPv6(src, dst net.IP, icmpType byte) []byte {
	pkt := make([]byte, 48) // 40 IPv6 header + 8 ICMPv6
	pkt[0] = 0x60           // version 6
	pkt[6] = 58             // next header ICMPv6
	copy(pkt[8:24], src.To16())
	copy(pkt[24:40], dst.To16())
	pkt[40] = icmpType
	return pkt
}

func TestIsICMPEchoReplyFrom(t *testing.T) {
	gw4 := net.ParseIP("198.18.0.0")
	other4 := net.ParseIP("198.18.0.5")
	client4 := net.ParseIP("198.18.0.2")

	gw6 := net.ParseIP("2001:2::")
	client6 := net.ParseIP("2001:2::2")

	tests := []struct {
		name   string
		packet []byte
		src    net.IP
		want   bool
	}{
		{"v4 echo reply from gateway", buildICMPv4(gw4, client4, icmpv4EchoReplyType), gw4, true},
		{"v4 echo request from gateway (not reply)", buildICMPv4(gw4, client4, 8), gw4, false},
		{"v4 echo reply from other src", buildICMPv4(other4, client4, icmpv4EchoReplyType), gw4, false},
		{"v6 echo reply from gateway", buildICMPv6(gw6, client6, icmpv6EchoReplyType), gw6, true},
		{"v6 echo request from gateway (not reply)", buildICMPv6(gw6, client6, 128), gw6, false},
		{"nil src", buildICMPv4(gw4, client4, icmpv4EchoReplyType), nil, false},
		{"empty packet", []byte{}, gw4, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsICMPEchoReplyFrom(tt.packet, tt.src); got != tt.want {
				t.Errorf("IsICMPEchoReplyFrom() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsICMPEchoReplyFrom_NonICMP(t *testing.T) {
	// A TCP packet from the gateway must not be mistaken for an echo reply.
	gw4 := net.ParseIP("198.18.0.0")
	pkt := buildICMPv4(gw4, net.ParseIP("198.18.0.2"), icmpv4EchoReplyType)
	pkt[9] = 6 // protocol TCP
	if IsICMPEchoReplyFrom(pkt, gw4) {
		t.Error("expected false for non-ICMP protocol")
	}
}
