package core

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

// TestReadFromConn_InjectsInterClientToSharedStack verifies the receive-side wiring: an inter-client
// packet (type == packetTypeToGvisor) read by a pool slot is injected DIRECTLY into the shared
// inter-client gvisor stack (interClientStack.InjectIP) — not dropped onto a bounded channel and
// not written to the TUN. Direct injection is what removed the old drop-if-full throughput
// collapse (docs/50) while keeping the stack decoupled from any single slot's lifetime.
//
// The proof of injection is an ICMP echo: a peer echo request to this client's TUN IP must be
// answered by the shared stack's ICMP forwarder, with the reply emerging on the stack's output
// channel (production: tunInbound). A packet that was dropped, or misrouted to tunOutbound, would
// produce no reply.
func TestReadFromConn_InjectsInterClientToSharedStack(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	defer serverSide.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverUDP, _ := NewUDPConnOverTCP(ctx, serverSide)

	out := make(chan *Packet, MaxSize) // stack output — production wires this to tunInbound
	tunOutbound := make(chan *Packet, MaxSize)
	ics := newInterClientStack(ctx, out, datagramHeaderLen)
	slot := &connSlot{
		id:          0,
		inbound:     make(chan *Packet, MaxSize),
		tunOutbound: tunOutbound,
		interClient: ics,
	}
	go slot.readFromConn(ctx, serverUDP, errChanOf())

	// Inter-client ICMP echo request from peer 10.0.0.5 to this client's TUN IP 198.18.0.2.
	req := genEchoRequest(t, "10.0.0.5", "198.18.0.2")
	if _, err := clientSide.Write(frameDatagram(packetTypeToGvisor, req)); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case pkt := <-out:
		if pkt == nil {
			t.Fatal("got nil packet")
		}
		reply := pkt.data[tunReserve : datagramHeaderLen+pkt.length]
		src, dst, _, err := netutil.ParseIPFast(reply)
		if err != nil {
			t.Fatalf("parse reply: %v", err)
		}
		if !src.Equal(net.ParseIP("198.18.0.2")) || !dst.Equal(net.ParseIP("10.0.0.5")) {
			t.Fatalf("echo reply addressing = %s->%s, want 198.18.0.2->10.0.0.5", src, dst)
		}
		config.LPool.Put(pkt.data[:])
	case <-tunOutbound:
		t.Fatal("inter-client packet must not be forwarded to tunOutbound")
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: inter-client packet was not injected into the shared stack (no echo reply)")
	}
}
