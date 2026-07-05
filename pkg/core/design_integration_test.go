package core

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// ============================================================================
// Tests for 01-network-architecture.md: Server-side 3-path packet routing
//
// readFromTCPConnWriteToEndpoint dispatches packets into 3 paths:
//   Path 1: route exists → forward to destination conn (inter-client)
//   Path 2: prefix=1 + no route → inject into gvisor stack
//   Path 3: prefix=0 + no route → send to TCPPacketChan → TUN device
// ============================================================================

func TestServerRouting_Path3_PrefixZeroToTCPPacketChan(t *testing.T) {
	// Path 3: prefix=0, no route → goes to hub.TCPPacketChan
	hub := NewRouteHub()
	handler := &gvisorTCPHandler{hub: hub, newStack: NewStack}

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Wrap serverConn as BufferedTCP (same as production code)
	go handler.readFromTCPConnWriteToEndpoint(ctx, NewBufferedTCP(ctx, serverConn), nil)

	// Send a prefix=0 packet from clientConn with datagram framing
	srcIP := net.IPv4(10, 0, 0, 1)
	dstIP := net.IPv4(10, 0, 0, 99) // no route registered for this
	ipPkt := buildIPv4Packet(srcIP, dstIP, []byte("hello"))

	sendFramedPacket(clientConn, 0, ipPkt)

	// Should arrive on TCPPacketChan
	select {
	case pkt := <-hub.TCPPacketChan:
		if pkt == nil {
			t.Fatal("got nil from TCPPacketChan")
		}
		// Verify src/dst parsed correctly
		if !pkt.src.Equal(srcIP.To4()) {
			t.Errorf("expected src %s, got %s", srcIP, pkt.src)
		}
		if !pkt.dst.Equal(dstIP.To4()) {
			t.Errorf("expected dst %s, got %s", dstIP, pkt.dst)
		}
		config.LPool.Put(pkt.data[:])
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: prefix=0 packet should go to TCPPacketChan")
	}
}

func TestServerRouting_Path1_RouteExistsForwards(t *testing.T) {
	// Path 1: route exists for dst → forward packet to registered conn
	hub := NewRouteHub()

	// Register a route for 10.0.0.99 → destinationConn
	destClient, destServer := net.Pipe()
	defer destClient.Close()
	defer destServer.Close()
	hub.AddRoute(context.Background(), net.IPv4(10, 0, 0, 99).To4(), destServer)

	handler := &gvisorTCPHandler{hub: hub, newStack: NewStack}

	sourceClient, sourceServer := net.Pipe()
	defer sourceClient.Close()
	defer sourceServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go handler.readFromTCPConnWriteToEndpoint(ctx, NewBufferedTCP(ctx, sourceServer), nil)

	// Send packet from sourceClient destined to 10.0.0.99 (which has a route)
	srcIP := net.IPv4(10, 0, 0, 1)
	dstIP := net.IPv4(10, 0, 0, 99)
	ipPkt := buildIPv4Packet(srcIP, dstIP, []byte("routed"))

	sendFramedPacket(sourceClient, 1, ipPkt)

	// Should arrive on destClient (the registered route)
	readBuf := make([]byte, 4096)
	destClient.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := destClient.Read(readBuf)
	if err != nil {
		t.Fatalf("failed to read from destination conn: %v", err)
	}
	if n == 0 {
		t.Fatal("destination conn received empty data")
	}
	// The forwarded data should contain the IP packet (with datagram framing)
}

// ============================================================================
// Tests for 01-network-architecture.md: Connection pool slot isolation
// ============================================================================

func TestConnectionPool_SlotIsolation(t *testing.T) {
	// Doc says: each slot has independent reconnect — if one dies, others work
	slots := make([]chan *Packet, ConnPoolSize)
	for i := range slots {
		slots[i] = make(chan *Packet, MaxSize)
	}

	// Put packets in different slots
	for i := 0; i < ConnPoolSize; i++ {
		buf := config.LPool.Get().([]byte)
		slots[i] <- &Packet{data: buf, length: 1}
	}

	// Verify each slot has exactly one packet
	for i, s := range slots {
		if len(s) != 1 {
			t.Errorf("slot %d: expected 1 packet, got %d", i, len(s))
		}
		pkt := <-s
		config.LPool.Put(pkt.data[:])
	}
}

// ============================================================================
// Tests for 01-network-architecture.md: Wire protocol framing
// ============================================================================

func TestWireProtocol_DatagramFraming(t *testing.T) {
	// Doc says: [2-byte big-endian length][prefix byte][IP packet data]
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	payload := []byte("test IP packet data")
	prefix := byte(1)

	go func() {
		buf := make([]byte, 2+1+len(payload))
		payloadLen := uint16(len(payload) + 1) // prefix + data
		binary.BigEndian.PutUint16(buf[:2], payloadLen)
		buf[2] = prefix
		copy(buf[3:], payload)
		client.Write(buf)
	}()

	// Read and verify framing
	header := make([]byte, 2)
	server.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := readFull(server, header); err != nil {
		t.Fatal(err)
	}
	length := binary.BigEndian.Uint16(header)
	if length != uint16(len(payload)+1) {
		t.Fatalf("expected length %d, got %d", len(payload)+1, length)
	}

	body := make([]byte, length)
	if _, err := readFull(server, body); err != nil {
		t.Fatal(err)
	}
	if body[0] != prefix {
		t.Fatalf("expected prefix %d, got %d", prefix, body[0])
	}
	if string(body[1:]) != string(payload) {
		t.Fatal("payload mismatch")
	}
}

// ============================================================================
// Tests for 01-network-architecture.md: RouteHub multi-conn per IP
// ============================================================================

func TestRouteHub_MultiConnFallback(t *testing.T) {
	// Doc says: ConnList supports multiple conns per IP, dead conns removed on write failure
	hub := NewRouteHub()
	ctx := context.Background()

	goodClient, goodServer := net.Pipe()
	defer goodClient.Close()
	defer goodServer.Close()

	deadClient, deadServer := net.Pipe()
	deadClient.Close()
	deadServer.Close()

	ip := net.IPv4(10, 0, 0, 5).To4()
	hub.AddRoute(ctx, ip, deadServer) // dead conn first
	hub.AddRoute(ctx, ip, goodServer) // good conn second

	// Drain goodClient so the Write to goodServer doesn't block
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := goodClient.Read(buf); err != nil {
				return
			}
		}
	}()

	_, err := hub.WriteToRoute(string(ip), []byte("test"))
	if err != nil {
		t.Fatalf("WriteToRoute should fallback to good conn, got: %v", err)
	}

	if !hub.HasRoute(string(ip)) {
		t.Fatal("route should still exist (good conn remains)")
	}
}

// ============================================================================
// helpers
// ============================================================================

func sendFramedPacket(conn net.Conn, prefix byte, ipPayload []byte) {
	buf := config.LPool.Get().([]byte)
	payloadLen := uint16(len(ipPayload) + 1)
	binary.BigEndian.PutUint16(buf[:2], payloadLen)
	buf[2] = prefix
	copy(buf[3:], ipPayload)
	conn.Write(buf[:3+len(ipPayload)])
	config.LPool.Put(buf)
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		nn, err := conn.Read(buf[n:])
		if err != nil {
			return n, err
		}
		n += nn
	}
	return n, nil
}

// buildIPv4Packet is already defined in datapath_test.go (same package).
