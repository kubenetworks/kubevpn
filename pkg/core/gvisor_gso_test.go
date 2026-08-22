package core

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// TestReadFromEndpoint_DropsOversizedPacket verifies the frame-boundary guard in
// readFromEndpointWriteToTCPConn: a packet too large to fit the [2 len][1 type][IP] frame is
// dropped (not truncated onto the wire), and the following normal packet is forwarded intact.
// This is the send-side counterpart to the TUN read guard covered in pkg/tun/batch_test.go.
func TestReadFromEndpoint_DropsOversizedPacket(t *testing.T) {
	endpoint := channel.New(10, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	h := &gvisorTCPHandler{}

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.readFromEndpointWriteToTCPConn(ctx, server, endpoint)

	// Oversized: tunReserve + Size() > LargeBufferSize -> cannot be framed -> must be dropped.
	oversized := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(make([]byte, config.LargeBufferSize)),
	})
	normalIP := buildIPv4Packet(net.ParseIP("10.0.0.1"), net.ParseIP("10.0.0.2"), []byte("hello"))
	normal := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(normalIP),
	})

	var pl stack.PacketBufferList
	pl.PushBack(oversized)
	pl.PushBack(normal)
	if _, err := endpoint.WritePackets(pl); err != nil {
		t.Fatalf("WritePackets: %v", err)
	}

	// The reader drops the oversized packet, so the first frame the client sees is the normal one.
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	prefix, ip, err := readDatagram(client)
	if err != nil {
		t.Fatalf("readDatagram: %v", err)
	}
	if prefix != packetTypeToTUN {
		t.Errorf("prefix = %d, want %d", prefix, packetTypeToTUN)
	}
	if !bytes.Equal(ip, normalIP) {
		t.Errorf("forwarded IP = %d bytes, want the %d-byte normal packet (oversized should have been dropped)", len(ip), len(normalIP))
	}
}
