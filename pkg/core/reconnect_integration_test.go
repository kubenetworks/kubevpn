package core

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// These tests exercise the reconnect hardening added across the data plane:
//
//	A — server read deadline evicts a stale "ghost" conn from the route table
//	B — bufferedTCP write deadline self-evicts a half-open conn, ConnList falls back
//	D — a freshly dialed slot proactively announces its TUN IP so the server registers
//	    the route immediately, without waiting for a data packet
//	E — ConnList prefers the most recently established conn, so a lingering ghost sinks
//	    to the tail and reverse traffic flows on the fresh conn straight away
//
// The shared failure they defend against: a client TCP conn drops and reconnects, but the
// server keeps writing reverse packets into the dead conn and the client never receives them.

// withShortKeepAlive temporarily shrinks config.KeepAliveTime so deadline-driven behavior is
// observable within a test's wall-clock budget. It restores the original on cleanup.
func withShortKeepAlive(t *testing.T, d time.Duration) {
	t.Helper()
	orig := config.KeepAliveTime
	config.KeepAliveTime = d
	t.Cleanup(func() { config.KeepAliveTime = orig })
}

// ---------------------------------------------------------------------------
// E — newest conn serves reverse traffic; the lingering ghost gets nothing
// ---------------------------------------------------------------------------

// countingConn records how many times it was written to. It never fails, modeling a conn that
// "accepts" writes (a healthy fresh conn, or a half-open ghost whose kernel still buffers).
type countingConn struct {
	net.Conn
	mu     sync.Mutex
	writes int
}

func (c *countingConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
	return len(b), nil
}

func (c *countingConn) writeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes
}

func (c *countingConn) Close() error                     { return nil }
func (c *countingConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *countingConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *countingConn) SetDeadline(time.Time) error      { return nil }
func (c *countingConn) SetReadDeadline(time.Time) error  { return nil }
func (c *countingConn) SetWriteDeadline(time.Time) error { return nil }

func TestReconnect_NewestConnServesReverseTraffic(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()
	clientIP := net.IPv4(198, 18, 0, 7).To4()

	// Phase 1: the client's original conn is registered (learned from its inbound traffic).
	ghost := &countingConn{}
	hub.AddRoute(ctx, clientIP, ghost)

	// Phase 2: the client reconnects; the fresh conn registers on the same route. Add prepends,
	// so the fresh conn leads and the ghost sinks to the tail.
	fresh := &countingConn{}
	hub.AddRoute(ctx, clientIP, fresh)

	// Phase 3: the server writes reverse traffic. It must reach the fresh conn, never the ghost —
	// even though the ghost would have silently "accepted" (and black-holed) the write.
	for i := 0; i < 5; i++ {
		if _, err := hub.WriteToRoute(string(clientIP), []byte("reverse")); err != nil {
			t.Fatalf("reverse write %d failed: %v", i, err)
		}
	}

	if fresh.writeCount() != 5 {
		t.Fatalf("fresh conn should serve all reverse traffic, got %d writes", fresh.writeCount())
	}
	if ghost.writeCount() != 0 {
		t.Fatalf("ghost conn must receive nothing, got %d writes", ghost.writeCount())
	}
}

// ---------------------------------------------------------------------------
// A — server read deadline evicts a stale conn from the route table
// ---------------------------------------------------------------------------

func TestReconnect_ReadDeadlineEvictsGhostRoute(t *testing.T) {
	// Real TCP so SetReadDeadline actually fires. Read deadline = KeepAliveTime*3.
	withShortKeepAlive(t, 120*time.Millisecond)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	hub := NewRouteHub()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h := &gvisorTCPHandler{hub: hub, newStack: NewStack, ctx: ctx, clients: make(map[string]*clientStack)}

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		h.readFromTCPConnWriteToEndpoint(ctx, NewBufferedTCP(ctx, conn))
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// One packet registers the route (server learns clientIP -> this conn), then the client goes
	// silent — simulating a slept/NAT-rebound client whose FIN never reaches the server.
	clientIP := net.IPv4(10, 0, 0, 42)
	sendFramedPacket(client, packetTypeToGvisor, buildIPv4Packet(clientIP, net.IPv4(10, 0, 0, 99), []byte("hi")))

	srcKey := string(clientIP.To4())
	waitFor(t, time.Second, func() bool { return hub.HasRoute(srcKey) },
		"route should be registered after first packet")

	// The read deadline (3*120ms = 360ms) must fire and evict the now-stale conn, instead of
	// leaving it to linger and swallow reverse traffic.
	waitFor(t, 3*time.Second, func() bool { return !hub.HasRoute(srcKey) },
		"read deadline should evict the stale route")
}

// ---------------------------------------------------------------------------
// B — bufferedTCP write deadline self-evicts a half-open conn; ConnList falls back
// ---------------------------------------------------------------------------

var errSimulatedTimeout = errors.New("i/o timeout (simulated)")

// blockingWriteConn models a half-open TCP socket: a Write never completes on its own, but honors
// the write deadline set on it (returning a timeout error), exactly as a real *net.TCPConn would.
type blockingWriteConn struct {
	net.Conn
	mu       sync.Mutex
	deadline time.Time
	closed   chan struct{}
	once     sync.Once
}

func newBlockingWriteConn() *blockingWriteConn {
	return &blockingWriteConn{closed: make(chan struct{})}
}

func (c *blockingWriteConn) SetWriteDeadline(tm time.Time) error {
	c.mu.Lock()
	c.deadline = tm
	c.mu.Unlock()
	return nil
}

func (c *blockingWriteConn) Write([]byte) (int, error) {
	c.mu.Lock()
	dl := c.deadline
	c.mu.Unlock()
	var fire <-chan time.Time
	if !dl.IsZero() {
		timer := time.NewTimer(time.Until(dl))
		defer timer.Stop()
		fire = timer.C
	}
	select {
	case <-fire:
		return 0, errSimulatedTimeout
	case <-c.closed:
		return 0, net.ErrClosed
	}
}

func (c *blockingWriteConn) Read([]byte) (int, error) { <-c.closed; return 0, net.ErrClosed }
func (c *blockingWriteConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}
func (c *blockingWriteConn) LocalAddr() net.Addr             { return &net.TCPAddr{} }
func (c *blockingWriteConn) RemoteAddr() net.Addr            { return &net.TCPAddr{} }
func (c *blockingWriteConn) SetReadDeadline(time.Time) error { return nil }
func (c *blockingWriteConn) SetDeadline(time.Time) error     { return nil }

func TestReconnect_WriteDeadlineEvictsGhostAndFallsBack(t *testing.T) {
	// run() reads config.KeepAliveTime at construction, so shrink it before NewBufferedTCP.
	withShortKeepAlive(t, 150*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rec := &countingConn{}
	fresh := NewBufferedTCP(ctx, rec)
	ghost := NewBufferedTCP(ctx, newBlockingWriteConn())

	// Add fresh first, then ghost: prepend puts the ghost at the head, so it is tried first —
	// forcing the write-deadline / fallback path to be exercised.
	cl := &ConnList{}
	cl.Add(fresh)
	cl.Add(ghost)

	mk := func() *Packet {
		buf := config.LPool.Get().([]byte)
		payload := []byte{packetTypeToGvisor, 0x45, 0x00}
		n := copy(buf[datagramHeaderLen:], payload)
		buf[0] = byte(n >> 8)
		buf[1] = byte(n)
		return NewPacket(buf, n, nil, nil)
	}

	// First write enqueues into the ghost's async buffer (writePacket only queues, so it "succeeds"
	// for now). That packet is doomed: run() will block on the half-open socket and time out.
	pkt := mk()
	conn, err := cl.WritePacket(pkt)
	pkt.release()
	if err != nil {
		t.Fatalf("first write should enqueue on the ghost: %v", err)
	}
	if conn != ghost {
		t.Fatal("first write should target the ghost (head of list)")
	}

	// Give the ghost's write deadline time to fire and close+mark the conn.
	time.Sleep(2 * config.KeepAliveTime)

	// Now the ghost reports closed (writePacket -> false), so ConnList evicts it and falls back to
	// the fresh conn, which actually delivers.
	pkt = mk()
	conn, err = cl.WritePacket(pkt)
	pkt.release()
	if err != nil {
		t.Fatalf("fallback write should succeed on the fresh conn: %v", err)
	}
	if conn != fresh {
		t.Fatal("write should fall back to the fresh conn after the ghost self-evicts")
	}
	if cl.Len() != 1 {
		t.Fatalf("ghost should be removed from the list, len=%d", cl.Len())
	}
	waitFor(t, time.Second, func() bool { return rec.writeCount() >= 1 },
		"fresh conn should receive the delivered packet")
}

// ---------------------------------------------------------------------------
// D — a freshly dialed slot proactively announces its TUN IP, and the server
//     registers the route from that announcement alone (no data packet)
// ---------------------------------------------------------------------------

func TestReconnect_ProactiveRegistration(t *testing.T) {
	clientTunIP := net.IPv4(198, 18, 0, 55).To4()
	regIP := buildIPv4Packet(clientTunIP, config.RouterIP, []byte("reg"))
	regPayload := append([]byte{packetTypeToGvisor}, regIP...)

	// Phase 1: a slot dials and must announce the registration on the new conn before anything else.
	clientConn, serverConn := net.Pipe()
	fwd := &Forwarder{
		Addr:        "test",
		MaxRetries:  1,
		Transporter: &mockTransporter{dialFn: func(context.Context, string) (net.Conn, error) { return clientConn, nil }},
		Connector:   &mockConnector{connectFn: func(ctx context.Context, c net.Conn) (net.Conn, error) { return NewUDPConnOverTCP(ctx, c) }},
	}
	slot := &connSlot{
		id:            1, // non-zero: any slot announces, not just slot 0
		inbound:       make(chan *Packet, MaxSize),
		tunOutbound:   make(chan *Packet, MaxSize),
		forward:       fwd,
		registrations: func() [][]byte { return [][]byte{regPayload} },
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	go slot.run(ctx1)

	type frame struct {
		prefix byte
		ip     []byte
		err    error
	}
	got := make(chan frame, 1)
	go func() {
		prefix, ip, err := readDatagram(serverConn)
		got <- frame{prefix, ip, err}
	}()

	select {
	case f := <-got:
		if f.err != nil {
			t.Fatalf("reading registration frame: %v", f.err)
		}
		if f.prefix != packetTypeToGvisor {
			t.Fatalf("registration prefix = %d, want %d", f.prefix, packetTypeToGvisor)
		}
		if !net.IP(f.ip[12:16]).Equal(clientTunIP) {
			t.Fatalf("registration src = %s, want %s", net.IP(f.ip[12:16]), clientTunIP)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("slot did not announce its route on connect")
	}
	cancel1()

	// Phase 2: the server handler registers the route from that announcement alone — no prior data.
	hub := NewRouteHub()
	cConn, sConn := net.Pipe()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	h := &gvisorTCPHandler{hub: hub, newStack: NewStack, ctx: ctx2, clients: make(map[string]*clientStack)}
	go h.readFromTCPConnWriteToEndpoint(ctx2, NewBufferedTCP(ctx2, sConn))

	sendFramedPacket(cConn, packetTypeToGvisor, regIP)
	waitFor(t, 2*time.Second, func() bool { return hub.HasRoute(string(clientTunIP)) },
		"server should register the route from the announcement, before any data packet")
}

// waitFor polls cond until it returns true or the timeout elapses, failing with msg otherwise.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}
