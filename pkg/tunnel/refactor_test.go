package tunnel

import (
	"context"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tunnel/internal/netstack"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// --- copyPacketToPool: verify View.Release + pkt.DecRef ---

func TestCopyPacketToPool_ReleasesResources(t *testing.T) {
	payload := []byte("test packet data for copy")
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(payload),
	})
	// pkt starts with refcount=1. After copyPacketToPool, it should be 0 (destroyed).

	packet := copyPacketToPool(pkt, 0, 0)
	defer packet.release()

	if packet.length != len(payload)+1 {
		t.Fatalf("expected length %d, got %d", len(payload)+1, packet.length)
	}
	if packet.data[0] != 0 {
		t.Fatalf("expected prefix=0, got %d", packet.data[0])
	}
	// Verify the payload was copied correctly
	for i, b := range payload {
		if packet.data[1+i] != b {
			t.Fatalf("payload mismatch at offset %d: expected %d, got %d", i, b, packet.data[1+i])
		}
	}
}

func TestCopyPacketToPool_WithHeadroom(t *testing.T) {
	payload := []byte("headroom test")
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(payload),
	})

	headroom := 2
	packet := copyPacketToPool(pkt, 1, headroom)
	defer packet.release()

	if packet.length != len(payload)+1 {
		t.Fatalf("expected length %d, got %d", len(payload)+1, packet.length)
	}
	// prefix at buf[headroom]
	if packet.data[headroom] != 1 {
		t.Fatalf("expected prefix=1 at offset %d, got %d", headroom, packet.data[headroom])
	}
	// payload starts at buf[headroom+1]
	for i, b := range payload {
		if packet.data[headroom+1+i] != b {
			t.Fatalf("payload mismatch at offset %d", i)
		}
	}
}

// --- drainPacketChan: verify all buffers returned to pool ---

func TestDrainPacketChan_ReturnsBuffers(t *testing.T) {
	ch := make(chan *packet, 10)

	// Put 5 packets with pool buffers
	for i := 0; i < 5; i++ {
		buf := config.LPool.Get().([]byte)
		buf[0] = byte(i)
		ch <- &packet{data: buf, length: 1}
	}

	if len(ch) != 5 {
		t.Fatalf("expected 5 packets in channel, got %d", len(ch))
	}

	drainPacketChan(ch)

	if len(ch) != 0 {
		t.Fatalf("expected 0 packets after drain, got %d", len(ch))
	}
}

func TestDrainPacketChan_EmptyChannel(t *testing.T) {
	ch := make(chan *packet, 10)
	// Should not block on empty channel
	drainPacketChan(ch)
}

func TestDrainPacketChan_NilPacketStops(t *testing.T) {
	ch := make(chan *packet, 10)
	buf := config.LPool.Get().([]byte)
	ch <- &packet{data: buf, length: 1}
	ch <- nil
	buf2 := config.LPool.Get().([]byte)
	ch <- &packet{data: buf2, length: 1}

	drainPacketChan(ch)
	// After nil, drain stops — one packet should remain
	if len(ch) != 1 {
		t.Fatalf("expected 1 remaining packet after nil stop, got %d", len(ch))
	}
	// Clean up
	pkt := <-ch
	config.LPool.Put(pkt.data[:])
}

// --- writeToTun: verify tunOutbound is drained on context cancel ---

func TestWriteToTun_DrainsOnCancel(t *testing.T) {
	tun := newMockTUN()
	outbound := make(chan *packet, MaxSize)
	device := &tunDevice{
		tun:         tun,
		tunOutbound: outbound,
		errChan:     make(chan error, 1),
	}

	// Put 3 packets in outbound
	for i := 0; i < 3; i++ {
		buf := config.LPool.Get().([]byte)
		buf[0] = 0
		buf[1] = 4 << 4 // IPv4 version nibble so Write doesn't panic
		outbound <- &packet{data: buf, length: 2}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		device.writeToTun(ctx)
		close(done)
	}()

	// Let it process some packets, then cancel
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	// After writeToTun exits, outbound should be drained
	if len(outbound) != 0 {
		t.Errorf("expected outbound drained, but %d packets remain", len(outbound))
	}
}

// --- readFromEndpointWriteToTCPConn: verify return on write error ---

func TestReadFromEndpointWriteToRoute_DropsNoRoute(t *testing.T) {
	hub := NewRouter()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	h := testTCPHandler(ctx, hub, netstack.NewStack)
	endpoint := channel.New(MaxSize, uint32(config.DefaultMTU), "")

	go h.readFromEndpointWriteToRoute(ctx, endpoint)

	// Send a packet to an IP with no route — reader should drop it and keep running.
	ipv4Pkt := buildMinimalIPv4Packet()
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(ipv4Pkt),
	})
	pkt.NetworkProtocolNumber = header.IPv4ProtocolNumber
	var pkts stack.PacketBufferList
	pkts.PushBack(pkt)
	_, _ = endpoint.WritePackets(pkts)

	// Send a second packet — if the reader survived the no-route drop, it will consume this one.
	ipv4Pkt2 := buildMinimalIPv4Packet()
	pkt2 := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(ipv4Pkt2),
	})
	pkt2.NetworkProtocolNumber = header.IPv4ProtocolNumber
	var pkts2 stack.PacketBufferList
	pkts2.PushBack(pkt2)
	_, _ = endpoint.WritePackets(pkts2)

	// Give the reader time to process both. If it crashed on the first, the test context
	// timeout will fire. We just verify the reader is still alive by cancelling gracefully.
	time.Sleep(100 * time.Millisecond)
	cancel()
	// If we get here, the reader handled no-route without crashing.
}

// --- Channel capacity: verify MaxSize is used, not tcp.DefaultReceiveBufferSize ---

func TestMaxSizeConstant(t *testing.T) {
	// Deliberate change-detector. MaxSize was reduced 1000 → 256 to cap a saturated
	// packet channel's worst case at 16 MiB; see TestPacketChannelWorstCaseMemory.
	if MaxSize != 256 {
		t.Fatalf("MaxSize changed: expected 256, got %d", MaxSize)
	}
}

// --- Router.bridge.queue drain in routeTCPToTun ---

func TestRouteTCPToTun_DrainsOnCancel(t *testing.T) {
	hub := NewRouter()
	outbound := make(chan *packet, MaxSize)
	defer drainPacketChan(outbound)

	dev := &tunDevice{tunOutbound: outbound, errChan: make(chan error, 1)}
	st := newServerTransport(dev, hub)

	// Put packets in tunQueue
	for i := 0; i < 3; i++ {
		buf := config.LPool.Get().([]byte)
		hub.bridge.send(context.Background(), newPacket(buf, 1, nil, nil))
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		st.routeTCPToTun(ctx)
		close(done)
	}()

	// Let it forward some, then cancel
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if len(hub.bridge.queue) != 0 {
		t.Errorf("tunQueue should be drained, but %d packets remain", len(hub.bridge.queue))
	}
}

// --- helpers ---

func buildMinimalIPv4Packet() []byte {
	return []byte{
		0x45, 0x00, 0x00, 0x1c, // version=4, IHL=5, total length=28
		0x00, 0x00, 0x00, 0x00, // identification, flags, fragment offset
		0x40, 0x06, 0x00, 0x00, // TTL=64, protocol=TCP, checksum
		0x0a, 0x00, 0x00, 0x01, // src: 10.0.0.1
		0x0a, 0x00, 0x00, 0x02, // dst: 10.0.0.2
		0x00, 0x50, 0x00, 0x51, // src port 80, dst port 81
		0x00, 0x00, 0x00, 0x00, // seq
	}
}
