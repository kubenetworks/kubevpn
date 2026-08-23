package core

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// TestReadFromConn_RoutesInterClientToSharedStack verifies the Fix B wiring: an inter-client
// packet (type == packetTypeToGvisor) read by a slot is routed to the transport-level shared
// interClientInbound channel — not to a per-slot stack and not to the TUN. This is what decouples
// the inter-client gvisor stack from any single slot's lifetime, so reconnecting a slot no longer
// destroys an in-flight inter-client transfer.
func TestReadFromConn_RoutesInterClientToSharedStack(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	defer serverSide.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	serverUDP, _ := NewUDPConnOverTCP(ctx, serverSide)

	interClient := make(chan *Packet, MaxSize)
	tunOutbound := make(chan *Packet, MaxSize)
	slot := &connSlot{
		id:                 0,
		inbound:            make(chan *Packet, MaxSize),
		tunOutbound:        tunOutbound,
		interClientInbound: interClient,
	}
	go slot.readFromConn(ctx, serverUDP, errChanOf())

	ip := buildIPv4Packet(net.IPv4(10, 0, 0, 5), net.IPv4(198, 18, 0, 2), []byte("interclient"))
	if _, err := clientSide.Write(frameDatagram(packetTypeToGvisor, ip)); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case pkt := <-interClient:
		if pkt == nil {
			t.Fatal("got nil packet")
		}
		got := pkt.data[tunReserve : datagramHeaderLen+pkt.length]
		if !bytes.Equal(got, ip) {
			t.Errorf("routed IP = %v, want %v", got, ip)
		}
		config.LPool.Put(pkt.data[:])
	case <-tunOutbound:
		t.Fatal("inter-client packet must not be forwarded to tunOutbound")
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: inter-client packet was not routed to the shared interClientInbound")
	}
}
