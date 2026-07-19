package core

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestHeartbeatStats_MarkReplyAndLastReply(t *testing.T) {
	var nilStats *HeartbeatStats
	if !nilStats.LastReply().IsZero() {
		t.Fatal("nil stats LastReply should be zero")
	}
	nilStats.MarkReply() // must not panic

	s := &HeartbeatStats{}
	if !s.LastReply().IsZero() {
		t.Fatal("fresh stats LastReply should be zero")
	}
	before := time.Now()
	s.MarkReply()
	got := s.LastReply()
	if got.Before(before) {
		t.Fatalf("LastReply %v should be at/after %v", got, before)
	}
}

func TestIsHeartbeatEchoReply(t *testing.T) {
	client4 := net.IPv4(198, 18, 0, 2)
	reply := buildICMPEchoReplyV4(config.RouterIP, client4)
	if !isHeartbeatEchoReply(reply) {
		t.Error("expected echo reply from RouterIP to be detected")
	}
	// Echo reply from a non-gateway source must not match.
	notGw := buildICMPEchoReplyV4(net.IPv4(10, 0, 0, 1), client4)
	if isHeartbeatEchoReply(notGw) {
		t.Error("echo reply from non-gateway must not match")
	}
}

// buildICMPEchoReplyV4 builds a minimal IPv4 ICMP echo reply packet (type 0).
func buildICMPEchoReplyV4(src, dst net.IP) []byte {
	pkt := make([]byte, 28)
	pkt[0] = 0x45
	pkt[9] = 1 // ICMP
	copy(pkt[12:16], src.To4())
	copy(pkt[16:20], dst.To4())
	pkt[20] = 0 // EchoReply
	return pkt
}

// TestReadFromConn_HeartbeatReplyMarksAndDrops drives connSlot.readFromConn over a pipe and
// verifies a gateway echo reply is recorded in stats and dropped (not forwarded to the TUN),
// while a normal packet is forwarded and leaves stats untouched.
func TestReadFromConn_HeartbeatReplyMarksAndDrops(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	defer serverSide.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	serverUDP, _ := NewUDPConnOverTCP(ctx, serverSide)

	stats := &HeartbeatStats{}
	tunOutbound := make(chan *Packet, MaxSize)
	slot := &connSlot{
		id:          0,
		inbound:     make(chan *Packet, MaxSize),
		tunOutbound: tunOutbound,
		stats:       stats,
	}
	go slot.readFromConn(ctx, serverUDP, errChanOf())

	// 1) Heartbeat echo reply from the gateway → recorded + dropped.
	reply := buildICMPEchoReplyV4(config.RouterIP, net.IPv4(198, 18, 0, 2))
	clientSide.Write(frameDatagram(0, reply))

	deadline := time.After(2 * time.Second)
	for stats.LastReply().IsZero() {
		select {
		case <-deadline:
			t.Fatal("timeout: heartbeat reply was not recorded")
		case <-tunOutbound:
			t.Fatal("heartbeat reply should be dropped, not forwarded to tunOutbound")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// 2) A normal (non-heartbeat) packet → forwarded, stats unchanged.
	prev := stats.LastReply()
	normal := buildIPv4Packet(net.IPv4(10, 0, 0, 5), net.IPv4(198, 18, 0, 2), []byte("hello"))
	clientSide.Write(frameDatagram(0, normal))

	select {
	case pkt := <-tunOutbound:
		if pkt == nil {
			t.Fatal("got nil packet")
		}
		config.LPool.Put(pkt.data[:])
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: normal packet should be forwarded to tunOutbound")
	}
	if !stats.LastReply().Equal(prev) {
		t.Fatal("normal packet must not update heartbeat stats")
	}
}

func errChanOf() chan error { return make(chan error, 2) }

// BenchmarkIsHeartbeatEchoReply measures the per-inbound-packet overhead added by the
// data-plane liveness observation in readFromConn. The "NormalPacket" case is the hot path
// (every inbound data packet) and should be a few ns with zero allocations, since the check
// short-circuits at the IP protocol byte for non-ICMP traffic.
func BenchmarkIsHeartbeatEchoReply_NormalPacket(b *testing.B) {
	// A typical MTU-sized inbound TCP/IPv4 data packet (protocol 6 — not ICMP).
	pkt := buildIPv4Packet(net.IPv4(10, 0, 0, 5), net.IPv4(198, 18, 0, 2), make([]byte, 1400))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if isHeartbeatEchoReply(pkt) {
			b.Fatal("data packet must not be detected as a heartbeat reply")
		}
	}
}

func BenchmarkIsHeartbeatEchoReply_EchoReply(b *testing.B) {
	// The rare case: an actual gateway echo reply (~1/min in production).
	pkt := buildICMPEchoReplyV4(config.RouterIP, net.IPv4(198, 18, 0, 2))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !isHeartbeatEchoReply(pkt) {
			b.Fatal("echo reply from RouterIP must be detected")
		}
	}
}
