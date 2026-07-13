package core

import (
	"context"
	"net"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// TestReadFromEndpoint_DropsOversizedPacket verifies the frame-boundary guard in
// readFromEndpointWriteToRoute: a packet too large to fit the [2 len][1 type][IP] frame is
// dropped (not truncated), and the following normal packet is routed via RouteHub.
func TestReadFromEndpoint_DropsOversizedPacket(t *testing.T) {
	hub := NewRouteHub()

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	clientIP := net.IPv4(198, 18, 0, 5).To4()
	hub.AddRoute(context.Background(), clientIP, server)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := &gvisorTCPHandler{hub: hub, ctx: ctx, clients: make(map[string]*clientStack)}
	endpoint := channel.New(10, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	go h.readFromEndpointWriteToRoute(ctx, endpoint)

	// Oversized: tunReserve + Size() > LargeBufferSize -> must be dropped.
	oversized := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(make([]byte, config.LargeBufferSize)),
	})
	normalIP := buildIPv4Packet(net.ParseIP("10.0.0.1"), clientIP, []byte("hello"))
	normal := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(normalIP),
	})
	normal.NetworkProtocolNumber = header.IPv4ProtocolNumber
	oversized.NetworkProtocolNumber = header.IPv4ProtocolNumber

	var pl stack.PacketBufferList
	pl.PushBack(oversized)
	pl.PushBack(normal)
	if _, err := endpoint.WritePackets(pl); err != nil {
		t.Fatalf("WritePackets: %v", err)
	}

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	prefix, ip, err := readDatagram(client)
	if err != nil {
		t.Fatalf("readDatagram: %v", err)
	}
	if prefix != packetTypeToTUN {
		t.Errorf("prefix = %d, want %d", prefix, packetTypeToTUN)
	}
	if len(ip) != len(normalIP) {
		t.Errorf("forwarded IP = %d bytes, want the %d-byte normal packet (oversized should have been dropped)", len(ip), len(normalIP))
	}
}
