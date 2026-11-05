package tunnel

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

// The control-connection reply path (R2, docs/50): the server answers heartbeat echo
// requests inline on the control conn, so liveness evidence no longer depends on the
// client's registered data connections. These tests wire the real handler, real framing
// and real TCP pairs — no mocked slot internals.

// controlReplySession starts the real handler on a TCP pair and issues the control
// declaration, leaving the conn ready to exchange heartbeat frames.
func controlReplySession(t *testing.T, h *gvisorTCPHandler) net.Conn {
	t.Helper()
	peer, raw := batchTCPPair(t, false)
	handled := make(chan struct{})
	go func() { h.Handle(context.Background(), raw); close(handled) }()
	t.Cleanup(func() {
		_ = peer.Close()
		select {
		case <-handled:
		case <-time.After(3 * time.Second):
			t.Error("control handler did not stop")
		}
	})
	// The declaration is consumed separately from the following heartbeats.
	sendFramedPacket(peer, packetTypeControl, nil)
	return peer
}

func controlReplyRequest(t *testing.T, src, dst net.IP) []byte {
	t.Helper()
	return genEchoRequest(t, src.String(), dst.String())
}

func controlReplyExpect(t *testing.T, peer net.Conn, request []byte) {
	t.Helper()
	_ = peer.SetReadDeadline(time.Now().Add(3 * time.Second))
	prefix, reply, err := readDatagram(peer)
	if err != nil {
		t.Fatalf("no heartbeat reply on control conn: %v", err)
	}
	if prefix != packetTypeToTUN {
		t.Fatalf("reply prefix = %d, want packetTypeToTUN (%d)", prefix, packetTypeToTUN)
	}
	if !isEchoReply(reply) {
		t.Fatalf("reply is not an echo reply: %x", reply)
	}
	if err := verifyEchoReplyChecksum(reply); err != nil {
		t.Fatalf("reply failed checksum validation (a real OS ping would drop it): %v", err)
	}
	// The ICMP body (ident/seq/payload) must be echoed verbatim so the client's
	// probe correlation matches.
	reqOff, replyOff := echoOffset(request), echoOffset(reply)
	if reqOff < 0 || replyOff < 0 {
		t.Fatal("request or reply is not a complete echo packet")
	}
	if len(request) != len(reply) {
		t.Fatalf("reply length %d, want %d", len(reply), len(request))
	}
	if string(request[reqOff+4:]) != string(reply[replyOff+4:]) {
		t.Fatal("reply changed the echo identity or payload")
	}
	// Addresses must be swapped: the reply comes from the gateway.
	if netutil.IsIPv4(request) {
		if !net.IP(request[16:20]).Equal(net.IP(reply[12:16])) || !net.IP(request[12:16]).Equal(net.IP(reply[16:20])) {
			t.Fatal("reply did not swap source and destination addresses")
		}
	} else {
		if !net.IP(request[24:40]).Equal(net.IP(reply[8:24])) || !net.IP(request[8:24]).Equal(net.IP(reply[24:40])) {
			t.Fatal("reply did not swap source and destination addresses")
		}
	}
}

func TestControlConn_InlineEchoReplyIPv4(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewRouter()
	h := GvisorLocalTCPHandler(hub).(*gvisorTCPHandler)
	h.Start(ctx)
	peer := controlReplySession(t, h)
	ip := net.IPv4(198, 18, 0, 71).To4()
	request := controlReplyRequest(t, ip, config.RouterIP)
	sendFramedPacket(peer, packetTypeControl, request)
	controlReplyExpect(t, peer, request)
	// The heartbeat must still create the client's per-client stack (lifecycle contract
	// kept from the pre-R2 injection path).
	h.mu.Lock()
	_, created := h.clients[string(ip)]
	h.mu.Unlock()
	if !created {
		t.Fatal("control heartbeat did not create the client stack")
	}
}

func TestControlConn_InlineEchoReplyIPv6(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewRouter()
	h := GvisorLocalTCPHandler(hub).(*gvisorTCPHandler)
	h.Start(ctx)
	peer := controlReplySession(t, h)
	ip := net.ParseIP("fd00::71")
	request := controlReplyRequest(t, ip, config.RouterIP6)
	sendFramedPacket(peer, packetTypeControl, request)
	controlReplyExpect(t, peer, request)
}

// TestControlConn_ReplySurvivesBlockedDataRoute is the R2 core assertion: with the client's
// registered data route wedged (writes never complete — the pre-R2 reply path would block
// here indefinitely), the heartbeat reply still arrives on the control conn.
func TestControlConn_ReplySurvivesBlockedDataRoute(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewRouter()
	h := GvisorLocalTCPHandler(hub).(*gvisorTCPHandler)
	h.Start(ctx)
	ip := net.IPv4(198, 18, 0, 72).To4()

	// A data route whose writer blocks until released. The pre-R2 stack-output reply
	// path routed heartbeat replies through exactly this route.
	_, deadRaw := batchTCPPair(t, false)
	dead := &lifetimeFailConn{Conn: deadRaw, entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(dead.release) }); _ = deadRaw.Close() })
	hub.AddRoute(ctx, ip, dead)

	peer := controlReplySession(t, h)
	request := controlReplyRequest(t, ip, config.RouterIP)
	sendFramedPacket(peer, packetTypeControl, request)
	controlReplyExpect(t, peer, request)

	// The blocked route writer was never touched by the heartbeat reply.
	select {
	case <-dead.entered:
		t.Fatal("heartbeat reply was routed through the blocked data connection")
	default:
	}
}

// TestControlConn_NonEchoRequestGetsNoReply pins the guard: anything that is not an
// ICMP echo request is dropped instead of answered (control conns carry only heartbeats).
func TestControlConn_NonEchoRequestGetsNoReply(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewRouter()
	h := GvisorLocalTCPHandler(hub).(*gvisorTCPHandler)
	h.Start(ctx)
	peer := controlReplySession(t, h)
	// A TCP packet to the gateway: valid IP, not ICMP.
	nonEcho := buildIPv4PacketWithPorts(net.IPv4(198, 18, 0, 73).To4(), net.IPv4(198, 18, 0, 254), 6, 1234, 80)
	sendFramedPacket(peer, packetTypeControl, nonEcho)
	_ = peer.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if prefix, reply, err := readDatagram(peer); err == nil {
		t.Fatalf("non-echo control frame got a reply: prefix=%d reply=%x", prefix, reply)
	}
}
