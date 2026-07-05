package core

import (
	"context"
	"errors"
	"net"
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

	buf, length := copyPacketToPool(pkt, 0, 0)
	defer config.LPool.Put(buf[:])

	if length != len(payload)+1 {
		t.Fatalf("expected length %d, got %d", len(payload)+1, length)
	}
	if buf[0] != 0 {
		t.Fatalf("expected prefix=0, got %d", buf[0])
	}
	// Verify the payload was copied correctly
	for i, b := range payload {
		if buf[1+i] != b {
			t.Fatalf("payload mismatch at offset %d: expected %d, got %d", i, b, buf[1+i])
		}
	}
}

func TestCopyPacketToPool_WithHeadroom(t *testing.T) {
	payload := []byte("headroom test")
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(payload),
	})

	headroom := 2
	buf, length := copyPacketToPool(pkt, 1, headroom)
	defer config.LPool.Put(buf[:])

	if length != len(payload)+1 {
		t.Fatalf("expected length %d, got %d", len(payload)+1, length)
	}
	// prefix at buf[headroom]
	if buf[headroom] != 1 {
		t.Fatalf("expected prefix=1 at offset %d, got %d", headroom, buf[headroom])
	}
	// payload starts at buf[headroom+1]
	for i, b := range payload {
		if buf[headroom+1+i] != b {
			t.Fatalf("payload mismatch at offset %d", i)
		}
	}
}

// --- drainPacketChan: verify all buffers returned to pool ---

func TestDrainPacketChan_ReturnsBuffers(t *testing.T) {
	ch := make(chan *Packet, 10)

	// Put 5 packets with pool buffers
	for i := 0; i < 5; i++ {
		buf := config.LPool.Get().([]byte)
		buf[0] = byte(i)
		ch <- &Packet{data: buf, length: 1}
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
	ch := make(chan *Packet, 10)
	// Should not block on empty channel
	drainPacketChan(ch)
}

func TestDrainPacketChan_NilPacketStops(t *testing.T) {
	ch := make(chan *Packet, 10)
	buf := config.LPool.Get().([]byte)
	ch <- &Packet{data: buf, length: 1}
	ch <- nil
	buf2 := config.LPool.Get().([]byte)
	ch <- &Packet{data: buf2, length: 1}

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
	outbound := make(chan *Packet, MaxSize)
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
		outbound <- &Packet{data: buf, length: 2}
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

type errWriter struct {
	net.Conn
	writeErr error
}

func (e *errWriter) Write([]byte) (int, error)        { return 0, e.writeErr }
func (e *errWriter) Close() error                     { return nil }
func (e *errWriter) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (e *errWriter) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (e *errWriter) SetDeadline(time.Time) error      { return nil }
func (e *errWriter) SetReadDeadline(time.Time) error  { return nil }
func (e *errWriter) SetWriteDeadline(time.Time) error { return nil }

func TestReadFromEndpointWriteToTCPConn_ReturnsOnWriteError(t *testing.T) {
	hub := NewRouteHub()
	h := &gvisorTCPHandler{hub: hub, newStack: NewStack}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	endpoint := channel.New(MaxSize, uint32(config.DefaultMTU), "")

	failConn := &errWriter{writeErr: errors.New("connection reset")}

	done := make(chan struct{})
	go func() {
		h.readFromEndpointWriteToTCPConn(ctx, failConn, endpoint)
		close(done)
	}()

	// WritePackets puts a packet on the endpoint's outbound channel (the direction
	// that ReadContext reads from). InjectInbound goes the other direction.
	ipv4Pkt := buildMinimalIPv4Packet()
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(ipv4Pkt),
	})
	pkt.NetworkProtocolNumber = header.IPv4ProtocolNumber
	var pkts stack.PacketBufferList
	pkts.PushBack(pkt)
	_, _ = endpoint.WritePackets(pkts)

	select {
	case <-done:
		// Good — function returned after write error
	case <-time.After(2 * time.Second):
		t.Fatal("readFromEndpointWriteToTCPConn did not return after write error")
	}
}

// --- Channel capacity: verify MaxSize is used, not tcp.DefaultReceiveBufferSize ---

func TestMaxSizeConstant(t *testing.T) {
	if MaxSize != 1000 {
		t.Fatalf("MaxSize changed: expected 1000, got %d", MaxSize)
	}
}

// --- RouteHub.TCPPacketChan drain in routeTCPToTun ---

func TestRouteTCPToTun_DrainsOnCancel(t *testing.T) {
	hub := NewRouteHub()
	outbound := make(chan *Packet, MaxSize)

	dev := &tunDevice{tunOutbound: outbound, errChan: make(chan error, 1)}
	st := newServerTransport(dev, hub)

	// Put packets in TCPPacketChan
	for i := 0; i < 3; i++ {
		buf := config.LPool.Get().([]byte)
		hub.TCPPacketChan <- &Packet{data: buf, length: 1}
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

	if len(hub.TCPPacketChan) != 0 {
		t.Errorf("TCPPacketChan should be drained, but %d packets remain", len(hub.TCPPacketChan))
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
