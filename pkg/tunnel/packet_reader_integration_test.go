package tunnel

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	framing "github.com/wencaiwulue/kubevpn/v2/pkg/internal/wire"
)

func TestClientPacketReaderCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name  string
		empty bool
		kind  byte
	}{
		{name: "empty-datagram-then-tun", empty: true, kind: packetTypeToTUN},
		{name: "reserved-kind-falls-back-to-tun", kind: 0x7f},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			client, peer := net.Pipe()
			framed, _ := framing.NewConn(ctx, client)
			outbound := make(chan *packet, 1)
			slot := &connSlot{tunOutbound: outbound, stats: &HeartbeatStats{}}
			errs := make(chan error, 1)
			done := make(chan struct{})
			go func() {
				(&connAttempt{connSlot: slot}).readGeneration(ctx, framed, errs, 0)
				close(done)
			}()
			defer func() {
				cancel()
				_ = framed.Close()
				_ = peer.Close()
				<-done
			}()
			if tc.empty {
				if _, err := peer.Write([]byte{0, 0}); err != nil {
					t.Fatal(err)
				}
			}
			ip := buildMinimalIPv4Packet()
			sendFramedPacket(peer, tc.kind, ip)
			select {
			case packet := <-outbound:
				if packet.kind() != tc.kind || !bytes.Equal(packet.ip(), ip) {
					t.Fatal("client fallback changed packet kind or IP payload")
				}
				packet.release()
			case err := <-errs:
				t.Fatalf("reader stopped after compatibility frame: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("reader did not forward packet after compatibility frame")
			}
		})
	}
}

func packetReaderServerSession(t *testing.T, h *gvisorTCPHandler) net.Conn {
	t.Helper()
	peer, raw := batchTCPPair(t, false)
	done := make(chan struct{})
	go func() { h.Handle(context.Background(), raw); close(done) }()
	t.Cleanup(func() {
		_ = peer.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("server handler did not stop")
		}
	})
	return peer
}

func TestServerDataConn_EmptyDatagramDoesNotEndStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := GvisorLocalTCPHandler(NewRouter()).(*gvisorTCPHandler)
	h.Start(ctx)
	peer := packetReaderServerSession(t, h)
	if _, err := peer.Write([]byte{0, 0}); err != nil {
		t.Fatal(err)
	}
	request := controlReplyRequest(t, net.IPv4(198, 18, 0, 74).To4(), config.RouterIP)
	sendFramedPacket(peer, packetTypeToGvisor, request)
	controlReplyExpect(t, peer, request)
}

func TestControlConn_FirstHeartbeatUsesLegacyDeclaration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewRouter()
	h := GvisorLocalTCPHandler(hub).(*gvisorTCPHandler)
	h.Start(ctx)
	peer := packetReaderServerSession(t, h)
	ip := net.IPv4(198, 18, 0, 75).To4()
	request := controlReplyRequest(t, ip, config.RouterIP)
	// Released clients could place a heartbeat in the first control datagram.
	// It is consumed as the role declaration; the next heartbeat is answered.
	sendFramedPacket(peer, packetTypeControl, request)
	sendFramedPacket(peer, packetTypeControl, request)
	controlReplyExpect(t, peer, request)
	if hub.HasRoute(string(ip)) {
		t.Fatal("control connection was registered as a data route")
	}
}
