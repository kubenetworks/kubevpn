package tunnel

import (
	"context"
	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"net"
	"testing"
)

func testTCPHandler(ctx context.Context, hub *Router, constructor stackConstructor) *gvisorTCPHandler {
	h := newTCPHandler(hub, constructor)
	h.Start(ctx)
	return h
}

func TestPeerService_ConnectionCancellationDoesNotOwnPeers(t *testing.T) {
	serviceCtx, stop := context.WithCancel(context.Background())
	defer stop()
	router := NewRouter()
	h := GvisorLocalTCPHandler(router).(*gvisorTCPHandler)
	h.Start(serviceCtx)
	defer h.Close()
	// A second factory shares the same registry rather than replacing callbacks.
	sibling := GvisorLocalTCPHandler(router).(*gvisorTCPHandler)
	sibling.Start(serviceCtx)
	if sibling.peerRegistry != h.peerRegistry {
		t.Fatal("listeners have different peer owners")
	}
	connCtx, cancelConn := context.WithCancel(serviceCtx)
	peer, raw := batchTCPPair(t, false)
	handled := make(chan struct{})
	go func() { defer close(handled); h.Handle(connCtx, raw) }()
	ip := net.IPv4(198, 18, 0, 77).To4()
	sendFramedPacket(peer, packetTypeToGvisor, genEchoRequest(t, ip.String(), config.RouterIP.String()))
	stage3Wait(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.clients[string(ip)] != nil
	}, "peer created through accepted connection")
	h.mu.Lock()
	retained := h.clients[string(ip)]
	h.mu.Unlock()
	cancelConn()
	lifetimeAwait(t, handled, "first connection canceled")
	select {
	case <-retained.done:
		t.Fatal("connection canceled service-owned peer")
	default:
	}
	freshPeer, freshRaw := batchTCPPair(t, false)
	fresh := newBufferedTCP(serviceCtx, freshRaw, nil)
	defer fresh.Close()
	router.AddRoute(serviceCtx, ip, fresh)
	if sibling.getOrCreateClientStack(string(ip)) != retained {
		t.Fatal("peer replaced after ordinary connection exit")
	}
	lifetimeHeartbeat(t, retained, freshPeer, ip)
	// Close must terminate and join every owned output/Destroy, not only cancel.
	if err := sibling.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-retained.done:
	default:
		t.Fatal("service Close returned before peer join")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	h.Start(context.Background())
	if h.getOrCreateClientStack(string(ip)) != nil {
		t.Fatal("terminal service restarted")
	}
}
