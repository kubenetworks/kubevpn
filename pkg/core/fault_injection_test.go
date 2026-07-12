package core

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// ============================================================================
// Sudden disconnect: client TCP conn dies mid-transfer
// ============================================================================

func TestFault_ClientDisconnect_ServerCleansUpRoute(t *testing.T) {
	hub := NewRouteHub()
	clientConn, serverConn := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	handler := &gvisorTCPHandler{hub: hub, newStack: NewStack, ctx: ctx, clients: make(map[string]*clientStack)}

	done := make(chan struct{})
	go func() {
		handler.readFromTCPConnWriteToEndpoint(ctx, NewBufferedTCP(ctx, serverConn))
		close(done)
	}()

	// Send one packet to register a route
	srcIP := net.IPv4(10, 0, 0, 42)
	dstIP := net.IPv4(10, 0, 0, 99)
	sendFramedPacket(clientConn, 0, buildIPv4Packet(srcIP, dstIP, []byte("hi")))

	// Wait for route to be registered
	time.Sleep(50 * time.Millisecond)
	srcKey := string(srcIP.To4())
	if !hub.HasRoute(srcKey) {
		t.Fatal("route should be registered after first packet")
	}

	// Simulate sudden disconnect
	clientConn.Close()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("readFromTCPConnWriteToEndpoint should exit after client disconnect")
	}

	// Route for disconnected client should be cleaned up (defer RemoveRoutesByConn)
	if hub.HasRoute(srcKey) {
		t.Fatal("route should be removed after client disconnect")
	}
}

func TestFault_ClientDisconnect_PoolBuffersNotLeaked(t *testing.T) {
	hub := NewRouteHub()
	clientConn, serverConn := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	handler := &gvisorTCPHandler{hub: hub, newStack: NewStack, ctx: ctx, clients: make(map[string]*clientStack)}

	done := make(chan struct{})
	go func() {
		handler.readFromTCPConnWriteToEndpoint(ctx, NewBufferedTCP(ctx, serverConn))
		close(done)
	}()

	// Send multiple packets
	for i := 0; i < 10; i++ {
		src := net.IPv4(10, 0, 0, byte(i+1))
		dst := net.IPv4(10, 0, 0, 99)
		sendFramedPacket(clientConn, 0, buildIPv4Packet(src, dst, []byte("data")))
	}
	time.Sleep(50 * time.Millisecond)

	// Crash the client
	clientConn.Close()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}

	// Drain TCPPacketChan to free pool buffers
	drainPacketChan(hub.TCPPacketChan)
}

// ============================================================================
// Slot failure: one connection pool slot dies, others continue
// ============================================================================

func TestFault_SlotDies_OtherSlotsWork(t *testing.T) {
	// Simulate ConnPoolSize=4 slots, kill slot 0, verify slot 1-3 still work
	n := ConnPoolSize
	slots := make([]chan *Packet, n)
	for i := range slots {
		slots[i] = make(chan *Packet, MaxSize)
	}

	// Send packets destined to different IPs (hash to different slots)
	var wg sync.WaitGroup
	received := make([]atomic.Int32, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(slotID int) {
			defer wg.Done()
			for {
				pkt, ok := <-slots[slotID]
				if !ok || pkt == nil {
					return
				}
				received[slotID].Add(1)
				config.LPool.Put(pkt.data[:])
			}
		}(i)
	}

	// Close slot 0 (simulate connection death)
	close(slots[0])

	// Send to remaining slots
	for i := 1; i < n; i++ {
		buf := config.LPool.Get().([]byte)
		slots[i] <- &Packet{data: buf, length: 1}
	}
	time.Sleep(50 * time.Millisecond)

	// Close remaining slots
	for i := 1; i < n; i++ {
		close(slots[i])
	}
	wg.Wait()

	for i := 1; i < n; i++ {
		if received[i].Load() != 1 {
			t.Errorf("slot %d should have received 1 packet, got %d", i, received[i].Load())
		}
	}
}

// ============================================================================
// Backpressure: TCPPacketChan full → producer doesn't hang indefinitely
// ============================================================================

func TestFault_TCPPacketChan_Backpressure(t *testing.T) {
	hub := NewRouteHub()

	// Fill TCPPacketChan to capacity
	for i := 0; i < MaxSize; i++ {
		buf := config.LPool.Get().([]byte)
		hub.TCPPacketChan <- &Packet{data: buf, length: 1}
	}

	// Now sending one more should block (channel full)
	// Use a goroutine with timeout to detect
	blocked := make(chan struct{})
	go func() {
		buf := config.LPool.Get().([]byte)
		hub.TCPPacketChan <- &Packet{data: buf, length: 1}
		close(blocked) // only reached if not blocked
	}()

	select {
	case <-blocked:
		t.Fatal("send should block when channel is full")
	case <-time.After(100 * time.Millisecond):
		// Expected: producer is blocked
	}

	// Drain one packet to unblock producer
	pkt := <-hub.TCPPacketChan
	config.LPool.Put(pkt.data[:])

	select {
	case <-blocked:
		// Good: unblocked after drain
	case <-time.After(time.Second):
		t.Fatal("producer should unblock after drain")
	}

	// Drain everything
	drainPacketChan(hub.TCPPacketChan)
}

// ============================================================================
// Context cancel during active transfer: goroutines exit, buffers freed
// ============================================================================

func TestFault_ContextCancel_WriteToTunExits(t *testing.T) {
	// Verifies writeToTun + handlePacket exit on context cancel and drain channels
	tun := newMockTUN()
	outbound := make(chan *Packet, MaxSize)
	device := &tunDevice{
		tun:         tun,
		tunOutbound: outbound,
		errChan:     make(chan error, 1),
	}

	// Pre-fill outbound with packets
	for i := 0; i < 5; i++ {
		buf := config.LPool.Get().([]byte)
		buf[0] = 0
		buf[1] = 0x45
		outbound <- &Packet{data: buf, length: 2}
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		device.writeToTun(ctx)
		close(done)
	}()

	// Let it process some, then abruptly cancel
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("writeToTun should exit on context cancel")
	}

	// outbound should be drained by defer
	if len(outbound) != 0 {
		t.Fatalf("outbound should be drained, %d remain", len(outbound))
	}
}

// ============================================================================
// Write error mid-stream: conn breaks during write, goroutine exits
// ============================================================================

type flakyConn struct {
	net.Conn
	writesBeforeFail int
	writeCount       int
	mu               sync.Mutex
}

func (c *flakyConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeCount++
	if c.writeCount > c.writesBeforeFail {
		return 0, errors.New("connection reset by peer")
	}
	return c.Conn.Write(b)
}

func TestFault_WriteFailsMidStream_CleanExit(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	// Wrap client with a conn that fails after 3 writes
	flaky := &flakyConn{Conn: client, writesBeforeFail: 3}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	slotCh := make(chan *Packet, MaxSize)
	errChan := make(chan error, 2)

	slot := &connSlot{id: 0, inbound: slotCh}
	done := make(chan struct{})
	go func() {
		slot.writeToConn(ctx, flaky, errChan)
		close(done)
	}()

	// Drain server side so writes don't block
	go func() {
		buf := make([]byte, 65536)
		for {
			if _, err := server.Read(buf); err != nil {
				return
			}
		}
	}()

	// Send 10 packets — should fail after 3
	for i := 0; i < 10; i++ {
		buf := config.LPool.Get().([]byte)
		buf[2] = 1
		n := copy(buf[3:], buildIPv4Packet(net.IPv4(10, 0, 0, 1), net.IPv4(10, 0, 0, 2), []byte("x")))
		select {
		case slotCh <- &Packet{data: buf, length: n + 1}:
		default:
			config.LPool.Put(buf)
		}
	}

	select {
	case <-done:
		// Good: writeToConn exited after write failure
	case <-time.After(3 * time.Second):
		t.Fatal("writeToConn should exit after write failure")
	}

	// Verify error was reported
	select {
	case err := <-errChan:
		if err == nil {
			t.Fatal("expected non-nil error")
		}
	default:
		t.Fatal("expected error in errChan")
	}

	// Drain remaining packets
	for len(slotCh) > 0 {
		pkt := <-slotCh
		config.LPool.Put(pkt.data[:])
	}
}

// ============================================================================
// Read timeout: server read deadline expires → clean disconnect
// ============================================================================

func TestFault_ReadTimeout_ServerDetectsDeadClient(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	// Set a very short read deadline on server side
	server.SetReadDeadline(time.Now().Add(100 * time.Millisecond))

	buf := make([]byte, 1024)
	_, err := server.Read(buf)
	if err == nil {
		t.Fatal("expected timeout error")
	}

	// Verify it's a timeout, not some other error
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("expected timeout error, got: %v", err)
	}
}

// ============================================================================
// RouteHub: all routes dead → cleanup + no panic on subsequent writes
// ============================================================================

func TestFault_AllRoutesDead_GracefulDegradation(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()

	// Register 3 dead connections
	ip := net.IPv4(10, 0, 0, 5).To4()
	for i := 0; i < 3; i++ {
		c1, c2 := net.Pipe()
		c1.Close()
		c2.Close()
		hub.AddRoute(ctx, ip, c2)
	}

	// Write should fail gracefully, not panic
	_, err := hub.WriteToRoute(string(ip), []byte("data"))
	if err == nil {
		t.Fatal("expected error when all routes are dead")
	}

	// After all dead conns removed, route should be gone
	if hub.HasRoute(string(ip)) {
		t.Fatal("route should be removed when all conns are dead")
	}

	// Subsequent writes to the same key should return error, not panic
	_, err = hub.WriteToRoute(string(ip), []byte("more data"))
	if err == nil {
		t.Fatal("expected error for removed route")
	}
}

// ============================================================================
// Concurrent disconnect + reconnect: no race conditions
// ============================================================================

func TestFault_ConcurrentDisconnectReconnect(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()
	ip := net.IPv4(10, 0, 0, 50).To4()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c1, c2 := net.Pipe()
			hub.AddRoute(ctx, ip, c1)
			time.Sleep(time.Millisecond)
			hub.RemoveRoutesByConn(ctx, c1)
			c1.Close()
			c2.Close()
		}()
	}
	wg.Wait()
	// No race condition = pass. Run with -race flag to verify.
}

// ============================================================================
// Heartbeat lost: client stops sending → server detects via read deadline
// ============================================================================

func TestFault_HeartbeatLost_ReadTimeoutDetects(t *testing.T) {
	// Uses real TCP so SetReadDeadline works (net.Pipe ignores deadlines)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	serverDone := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		ctx := context.Background()
		udpConn, _ := NewUDPConnOverTCP(ctx, conn)

		// First read — should succeed
		buf := config.LPool.Get().([]byte)
		_, err = udpConn.Read(buf)
		config.LPool.Put(buf)
		if err != nil {
			serverDone <- err
			return
		}

		// Set short deadline, wait for heartbeat that never comes
		conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		buf2 := config.LPool.Get().([]byte)
		_, err = udpConn.Read(buf2)
		config.LPool.Put(buf2)
		serverDone <- err
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// Send one packet, then go silent
	ip := buildIPv4Packet(net.IPv4(10, 0, 0, 1), net.IPv4(10, 0, 0, 2), []byte("last"))
	buf := make([]byte, 2+1+len(ip))
	binary.BigEndian.PutUint16(buf[:2], uint16(len(ip)+1))
	buf[2] = 1
	copy(buf[3:], ip)
	client.Write(buf)

	// Server should detect timeout
	select {
	case err := <-serverDone:
		if err == nil {
			t.Fatal("server should detect heartbeat loss via read timeout")
		}
		var netErr net.Error
		if !errors.As(err, &netErr) || !netErr.Timeout() {
			t.Fatalf("expected timeout error, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for server to detect heartbeat loss")
	}
}

// ============================================================================
// writeToTun: TUN device error → reports via errChan, drains remaining
// ============================================================================

func TestFault_TunDeviceError_ReportsAndDrains(t *testing.T) {
	errTun := &errOnWriteTUN{err: errors.New("device removed")}
	outbound := make(chan *Packet, MaxSize)
	errChan := make(chan error, 1)
	device := &tunDevice{
		tun:         errTun,
		tunOutbound: outbound,
		errChan:     errChan,
	}

	// Put packets in outbound (some will remain after error)
	for i := 0; i < 5; i++ {
		buf := config.LPool.Get().([]byte)
		buf[0] = 0
		buf[1] = 0x45 // IPv4
		outbound <- &Packet{data: buf, length: 2}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		device.writeToTun(ctx)
		close(done)
	}()

	// Should exit with error
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("writeToTun should exit on TUN write error")
	}

	// Error should be reported
	select {
	case err := <-errChan:
		if err == nil || err.Error() != "device removed" {
			t.Fatalf("expected 'device removed', got: %v", err)
		}
	default:
		t.Fatal("expected error in errChan")
	}

	// Remaining packets should be drained (defer drainPacketChan)
	if len(outbound) != 0 {
		t.Fatalf("outbound should be drained, but %d packets remain", len(outbound))
	}
}

// --- helpers ---

type errOnWriteTUN struct {
	net.Conn
	err error
}

func (e *errOnWriteTUN) Write([]byte) (int, error)        { return 0, e.err }
func (e *errOnWriteTUN) Read([]byte) (int, error)         { return 0, io.EOF }
func (e *errOnWriteTUN) Close() error                     { return nil }
func (e *errOnWriteTUN) LocalAddr() net.Addr              { return &net.IPAddr{IP: net.IPv4(198, 18, 0, 1)} }
func (e *errOnWriteTUN) RemoteAddr() net.Addr             { return &net.IPAddr{IP: net.IPv4(198, 18, 0, 1)} }
func (e *errOnWriteTUN) SetDeadline(time.Time) error      { return nil }
func (e *errOnWriteTUN) SetReadDeadline(time.Time) error  { return nil }
func (e *errOnWriteTUN) SetWriteDeadline(time.Time) error { return nil }
