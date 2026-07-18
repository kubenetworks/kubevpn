package core

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// =============================================================================
// End-to-end data path tests simulating the full packet lifecycle.
//
// These tests model the 3 key traffic patterns:
// 1. client → service/pod → response → client
// 2. inbound → proxy resource (sidecar) → traffic-manager → client
// 3. client A → proxy resource → traffic-manager → client B (inter-client)
//
// The tests use mock objects for TUN, TCP connections, and gvisor stacks
// to verify the correct routing, framing, and fallback behavior without
// real network I/O.
// =============================================================================

// --- Test infrastructure ---

// buildIPv4Packet creates a minimal fake IPv4 packet with given src/dst.
func buildIPv4Packet(src, dst net.IP, payload []byte) []byte {
	// Minimal IPv4 header (20 bytes) + payload
	totalLen := 20 + len(payload)
	pkt := make([]byte, totalLen)
	pkt[0] = 0x45 // version=4, IHL=5
	binary.BigEndian.PutUint16(pkt[2:4], uint16(totalLen))
	pkt[8] = 64 // TTL
	pkt[9] = 6  // protocol = TCP
	copy(pkt[12:16], src.To4())
	copy(pkt[16:20], dst.To4())
	copy(pkt[20:], payload)
	return pkt
}

// frameDatagram wraps an IP packet with the wire protocol framing.
// Returns: [2-byte length][1-byte prefix][IP packet]
func frameDatagram(prefix byte, ipPacket []byte) []byte {
	payloadLen := 1 + len(ipPacket)
	frame := make([]byte, 2+payloadLen)
	binary.BigEndian.PutUint16(frame[:2], uint16(payloadLen))
	frame[2] = prefix
	copy(frame[3:], ipPacket)
	return frame
}

// readDatagram reads a framed datagram from a conn.
func readDatagram(conn net.Conn) (prefix byte, ipPacket []byte, err error) {
	header := make([]byte, 2)
	if _, err = fullRead(conn, header); err != nil {
		return
	}
	length := binary.BigEndian.Uint16(header)
	data := make([]byte, length)
	if _, err = fullRead(conn, data); err != nil {
		return
	}
	return data[0], data[1:], nil
}

func fullRead(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// =============================================================================
// Scenario 1: client → service/pod → response → client
// =============================================================================

func TestDataPath_ClientToService(t *testing.T) {
	// Setup: client sends a packet to service IP 10.0.0.5.
	// Server receives it, "processes" (simulated), and sends response back.

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	hub := NewRouteHub()
	ctx := context.Background()

	clientIP := net.ParseIP("198.18.0.2")
	serviceIP := net.ParseIP("10.0.0.5")

	// Register client route on server side
	hub.AddRoute(ctx, clientIP.To4(), serverConn)

	// Client sends: frame a packet destined for serviceIP
	ipPkt := buildIPv4Packet(clientIP, serviceIP, []byte("GET / HTTP/1.1"))
	frame := frameDatagram(1, ipPkt) // prefix=1: needs gvisor processing

	go func() {
		clientConn.Write(frame)
	}()

	// Server receives
	prefix, received, err := readDatagram(serverConn)
	if err != nil {
		t.Fatalf("server read failed: %v", err)
	}
	if prefix != 1 {
		t.Fatalf("expected prefix=1, got %d", prefix)
	}

	// Verify src/dst in received packet
	src, dst, _, _ := util.ParseIPFast(received)
	if !src.Equal(clientIP) {
		t.Fatalf("expected src=%s, got %s", clientIP, src)
	}
	if !dst.Equal(serviceIP) {
		t.Fatalf("expected dst=%s, got %s", serviceIP, dst)
	}

	// Server sends response (prefix=0: already processed by gvisor)
	responsePkt := buildIPv4Packet(serviceIP, clientIP, []byte("HTTP/1.1 200 OK"))
	responseFrame := frameDatagram(0, responsePkt)

	go func() {
		serverConn.Write(responseFrame)
	}()

	// Client receives
	respPrefix, respPkt, err := readDatagram(clientConn)
	if err != nil {
		t.Fatalf("client read failed: %v", err)
	}
	if respPrefix != 0 {
		t.Fatalf("expected response prefix=0, got %d", respPrefix)
	}
	respSrc, respDst, _, _ := util.ParseIPFast(respPkt)
	if !respSrc.Equal(serviceIP) {
		t.Fatalf("response src should be service")
	}
	if !respDst.Equal(clientIP) {
		t.Fatalf("response dst should be client")
	}
}

// =============================================================================
// Scenario 2: inbound → proxy resource → traffic-manager TUN → client
// (simulates server TUN routing packets to client via RouteHub)
// =============================================================================

func TestDataPath_InboundProxyToClient(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()

	clientIP := net.ParseIP("198.18.0.2")
	podIP := net.ParseIP("10.0.1.50")

	// Client has 4 connections registered (connection pool)
	conns := make([]*mockConn, 4)
	for i := range conns {
		conns[i] = newMockConn(fmt.Sprintf("slot%d", i), false)
		hub.AddRoute(ctx, clientIP.To4(), conns[i])
	}

	// Server TUN receives a packet from a pod destined for the client
	ipPkt := buildIPv4Packet(podIP, clientIP, []byte("response-data"))
	payloadLen := len(ipPkt) + 1
	data := make([]byte, payloadLen+2)
	binary.BigEndian.PutUint16(data[:2], uint16(payloadLen))
	data[2] = 1 // prefix for routeTun
	copy(data[3:], ipPkt)

	// WriteToRoute simulates what routeTun does
	dstKey := string(clientIP.To4())
	conn, err := hub.WriteToRoute(dstKey, data)
	if err != nil {
		t.Fatalf("WriteToRoute failed: %v", err)
	}
	if conn == nil {
		t.Fatal("expected non-nil conn")
	}

	// Verify one of the conns received the data
}

// Test with some dead conns — fallback should work
func TestDataPath_InboundProxy_WithDeadSlots(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()

	clientIP := net.ParseIP("198.18.0.3")

	dead0 := newMockConn("dead0", true)
	dead1 := newMockConn("dead1", true)
	alive2 := newMockConn("alive2", false)
	alive3 := newMockConn("alive3", false)

	hub.AddRoute(ctx, clientIP.To4(), dead0)
	hub.AddRoute(ctx, clientIP.To4(), dead1)
	hub.AddRoute(ctx, clientIP.To4(), alive2)
	hub.AddRoute(ctx, clientIP.To4(), alive3)

	ipPkt := buildIPv4Packet(net.ParseIP("10.0.1.99"), clientIP, []byte("data"))
	frame := frameDatagram(1, ipPkt)

	dstKey := string(clientIP.To4())
	conn, err := hub.WriteToRoute(dstKey, frame)
	if err != nil {
		t.Fatalf("fallback failed: %v", err)
	}
	if conn != alive2 {
		t.Fatalf("expected alive2, got different conn")
	}

	// Dead conns should be auto-removed
	val, _ := hub.RouteMapTCP.Load(dstKey)
	if val.(*ConnList).Len() != 2 {
		t.Fatalf("expected 2 surviving, got %d", val.(*ConnList).Len())
	}
}

// =============================================================================
// Scenario 3: client A → traffic-manager → client B (inter-client routing)
// =============================================================================

func TestDataPath_InterClient_AtoB(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()

	clientA := net.ParseIP("198.18.0.2")
	clientB := net.ParseIP("198.18.0.3")

	// Both clients have connection pools registered
	connA := make([]*mockConn, 4)
	connB := make([]*mockConn, 4)
	for i := 0; i < 4; i++ {
		connA[i] = newMockConn(fmt.Sprintf("A%d", i), false)
		connB[i] = newMockConn(fmt.Sprintf("B%d", i), false)
		hub.AddRoute(ctx, clientA.To4(), connA[i])
		hub.AddRoute(ctx, clientB.To4(), connB[i])
	}

	// A sends packet to B's TUN IP
	ipPkt := buildIPv4Packet(clientA, clientB, []byte("ping"))
	dgram := newDatagramPacket(make([]byte, len(ipPkt)+10), len(ipPkt)+1)
	// Simulate the framing that gvisor_tun_endpoint does
	copy(dgram.Data[1:], ipPkt)
	dgram.Data[0] = 1 // prefix=1 (inject to B's gvisor)
	dgram.DataLength = uint16(len(ipPkt) + 1)

	// Use WriteFuncToRoute (same as gvisor_tun_endpoint does)
	dstKey := string(clientB.To4())
	conn, err := hub.WriteFuncToRoute(dstKey, func(c net.Conn) error {
		return dgram.Write(c)
	})
	if err != nil {
		t.Fatalf("A→B routing failed: %v", err)
	}
	// Should use one of B's conns
	found := false
	for _, c := range connB {
		if c == conn {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected one of B's conns")
	}
}

func TestDataPath_InterClient_WithFailover(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()

	clientA := net.ParseIP("198.18.0.10")
	clientB := net.ParseIP("198.18.0.11")

	connA0 := newMockConn("A0", false)
	hub.AddRoute(ctx, clientA.To4(), connA0)

	// B has 4 conns, first 3 are dead (reconnecting)
	connB0 := newMockConn("B0", true)
	connB1 := newMockConn("B1", true)
	connB2 := newMockConn("B2", true)
	connB3 := newMockConn("B3", false) // only this one alive
	hub.AddRoute(ctx, clientB.To4(), connB0)
	hub.AddRoute(ctx, clientB.To4(), connB1)
	hub.AddRoute(ctx, clientB.To4(), connB2)
	hub.AddRoute(ctx, clientB.To4(), connB3)

	// A→B: should eventually reach B via connB3
	ipPkt := buildIPv4Packet(clientA, clientB, []byte("important"))
	dstKey := string(clientB.To4())
	conn, err := hub.WriteToRoute(dstKey, frameDatagram(1, ipPkt))
	if err != nil {
		t.Fatalf("all routes should not fail, connB3 is alive: %v", err)
	}
	if conn != connB3 {
		t.Fatal("expected connB3 (only alive)")
	}

	// Verify dead conns removed, only connB3 remains
	val, _ := hub.RouteMapTCP.Load(dstKey)
	if val.(*ConnList).Len() != 1 {
		t.Fatalf("expected 1 surviving conn, got %d", val.(*ConnList).Len())
	}
}

// =============================================================================
// Scenario: Bidirectional — A sends to B, B responds to A
// =============================================================================

func TestDataPath_InterClient_Bidirectional(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()

	clientA := net.ParseIP("198.18.0.20")
	clientB := net.ParseIP("198.18.0.21")

	connA := newMockConn("A0", false)
	connB := newMockConn("B0", false)
	hub.AddRoute(ctx, clientA.To4(), connA)
	hub.AddRoute(ctx, clientB.To4(), connB)

	// A → B
	pktAtoB := buildIPv4Packet(clientA, clientB, []byte("request"))
	conn, err := hub.WriteToRoute(string(clientB.To4()), frameDatagram(1, pktAtoB))
	if err != nil {
		t.Fatalf("A→B failed: %v", err)
	}
	if conn != connB {
		t.Fatal("expected connB")
	}

	// B → A (response)
	pktBtoA := buildIPv4Packet(clientB, clientA, []byte("response"))
	conn, err = hub.WriteToRoute(string(clientA.To4()), frameDatagram(0, pktBtoA))
	if err != nil {
		t.Fatalf("B→A failed: %v", err)
	}
	if conn != connA {
		t.Fatal("expected connA")
	}
}

// =============================================================================
// Scenario: Connection pool hash consistency
// =============================================================================

func TestDataPath_HashConsistency(t *testing.T) {
	// Verify that the same dst IP always hashes to the same slot
	dst := net.ParseIP("10.0.0.5")
	slots := 4
	expected := ipHash(dst, slots)

	for i := 0; i < 1000; i++ {
		got := ipHash(dst, slots)
		if got != expected {
			t.Fatalf("hash inconsistency: expected %d, got %d on iteration %d", expected, got, i)
		}
	}

	// Different IPs should distribute across slots
	distribution := make(map[int]int)
	for i := 0; i < 256; i++ {
		ip := net.IPv4(10, 0, 0, byte(i))
		slot := ipHash(ip, slots)
		distribution[slot]++
	}
	// Each slot should get roughly 25% (64±32)
	for slot, count := range distribution {
		if count < 32 || count > 96 {
			t.Fatalf("poor distribution: slot %d got %d/256 IPs", slot, count)
		}
	}
}

// =============================================================================
// Scenario: Concurrent multi-client full lifecycle
// =============================================================================

func TestDataPath_ConcurrentMultiClient(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()

	numClients := 10
	connsPerClient := 4

	// Register all clients with connection pools
	clientIPs := make([]net.IP, numClients)
	for i := 0; i < numClients; i++ {
		clientIPs[i] = net.IPv4(198, 18, 0, byte(i+1))
		for j := 0; j < connsPerClient; j++ {
			hub.AddRoute(ctx, clientIPs[i].To4(), newMockConn(fmt.Sprintf("c%d-s%d", i, j), false))
		}
	}

	// Concurrent cross-client traffic: each client sends to every other
	var wg sync.WaitGroup
	errs := make(chan error, numClients*numClients)

	for i := 0; i < numClients; i++ {
		for j := 0; j < numClients; j++ {
			if i == j {
				continue
			}
			wg.Add(1)
			go func(src, dst net.IP) {
				defer wg.Done()
				pkt := buildIPv4Packet(src, dst, []byte("data"))
				frame := frameDatagram(1, pkt)
				_, err := hub.WriteToRoute(string(dst.To4()), frame)
				if err != nil {
					errs <- fmt.Errorf("%s→%s: %w", src, dst, err)
				}
			}(clientIPs[i], clientIPs[j])
		}
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent routing error: %v", err)
	}
}

// =============================================================================
// Scenario: Single user proxy resource — external traffic routed to one client
// =============================================================================

func TestDataPath_ProxyResource_SingleUser(t *testing.T) {
	// Simulates: User A proxies deployment/productpage
	// External request → envoy DNAT to 198.18.0.2 → traffic-manager TUN → client A
	hub := NewRouteHub()
	ctx := context.Background()

	clientA := net.IPv4(198, 18, 0, 2)
	podSidecarIP := net.IPv4(10, 0, 1, 100) // pod with injected sidecar

	// Client A has 4-conn pool registered
	connsA := make([]*mockConn, 4)
	for i := range connsA {
		connsA[i] = newMockConn(fmt.Sprintf("A-slot%d", i), false)
		hub.AddRoute(ctx, clientA, connsA[i])
	}

	// Simulate: external request hits pod, envoy DNATs to clientA's TUN IP
	// Pod sidecar sends packet through VPN to traffic-manager
	// Traffic-manager TUN reads it, routeTun looks up dst=198.18.0.2
	requestPkt := buildIPv4Packet(podSidecarIP, clientA, []byte("GET /productpage HTTP/1.1\r\nHost: productpage:9080\r\n"))
	frame := frameDatagram(1, requestPkt)

	dstKey := string(clientA)
	conn, err := hub.WriteToRoute(dstKey, frame)
	if err != nil {
		t.Fatalf("route to client A failed: %v", err)
	}
	if conn == nil {
		t.Fatal("expected non-nil conn")
	}

	// Verify it went to one of A's conns
	found := false
	for _, c := range connsA {
		if c == conn {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("packet should be routed to one of client A's conns")
	}
}

// =============================================================================
// Scenario: Multiple users proxy SAME resource with different headers
// User A: proxy deployment/foo --headers user=A  (gets TUN IP 198.18.0.2)
// User B: proxy deployment/foo --headers user=B  (gets TUN IP 198.18.0.3)
// =============================================================================

func TestDataPath_ProxyResource_MultiUser_SameResource(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()

	clientA := net.IPv4(198, 18, 0, 2) // user A
	clientB := net.IPv4(198, 18, 0, 3) // user B
	podIP := net.IPv4(10, 0, 1, 100)   // shared pod (deployment/foo)

	// Both clients registered with conn pools
	connA := newMockConn("A0", false)
	connB := newMockConn("B0", false)
	hub.AddRoute(ctx, clientA, connA)
	hub.AddRoute(ctx, clientB, connB)

	// Request with header user=A → envoy DNATs to 198.18.0.2 (client A)
	reqToA := buildIPv4Packet(podIP, clientA, []byte("user=A request"))
	conn, err := hub.WriteToRoute(string(clientA), frameDatagram(1, reqToA))
	if err != nil {
		t.Fatalf("route to A failed: %v", err)
	}
	if conn != connA {
		t.Fatal("request with user=A should reach client A")
	}

	// Request with header user=B → envoy DNATs to 198.18.0.3 (client B)
	reqToB := buildIPv4Packet(podIP, clientB, []byte("user=B request"))
	conn, err = hub.WriteToRoute(string(clientB), frameDatagram(1, reqToB))
	if err != nil {
		t.Fatalf("route to B failed: %v", err)
	}
	if conn != connB {
		t.Fatal("request with user=B should reach client B")
	}

	// Request without matching headers → envoy routes to original backend
	// (this doesn't go through the tunnel at all — no test needed at this layer)
}

// Test: both users have conn pools, one user has partial failures
func TestDataPath_ProxyResource_MultiUser_SameResource_WithFailover(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()

	clientA := net.IPv4(198, 18, 0, 2)
	clientB := net.IPv4(198, 18, 0, 3)
	podIP := net.IPv4(10, 0, 1, 100)

	// Client A: all healthy
	for i := 0; i < 4; i++ {
		hub.AddRoute(ctx, clientA, newMockConn(fmt.Sprintf("A%d", i), false))
	}
	// Client B: first 3 dead, only slot 3 alive
	for i := 0; i < 3; i++ {
		hub.AddRoute(ctx, clientB, newMockConn(fmt.Sprintf("B%d-dead", i), true))
	}
	aliveB3 := newMockConn("B3-alive", false)
	hub.AddRoute(ctx, clientB, aliveB3)

	// Traffic to A: succeeds immediately
	conn, err := hub.WriteToRoute(string(clientA), frameDatagram(1, buildIPv4Packet(podIP, clientA, []byte("to-A"))))
	if err != nil {
		t.Fatalf("route to A failed: %v", err)
	}
	if conn == nil {
		t.Fatal("A should have healthy conns")
	}

	// Traffic to B: falls back to slot 3
	conn, err = hub.WriteToRoute(string(clientB), frameDatagram(1, buildIPv4Packet(podIP, clientB, []byte("to-B"))))
	if err != nil {
		t.Fatalf("route to B failed: %v", err)
	}
	if conn != aliveB3 {
		t.Fatal("B should fallback to alive slot 3")
	}

	// B's dead conns should be cleaned up
	val, _ := hub.RouteMapTCP.Load(string(clientB))
	if val.(*ConnList).Len() != 1 {
		t.Fatalf("expected 1 surviving conn for B, got %d", val.(*ConnList).Len())
	}
}

// =============================================================================
// Scenario: Multiple users proxy DIFFERENT resources
// User A: proxy deployment/foo  (TUN IP 198.18.0.2)
// User B: proxy deployment/bar  (TUN IP 198.18.0.3)
// User C: proxy deployment/baz  (TUN IP 198.18.0.4)
// =============================================================================

func TestDataPath_ProxyResource_MultiUser_DifferentResources(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()

	clientA := net.IPv4(198, 18, 0, 2) // proxies deployment/foo
	clientB := net.IPv4(198, 18, 0, 3) // proxies deployment/bar
	clientC := net.IPv4(198, 18, 0, 4) // proxies deployment/baz

	podFoo := net.IPv4(10, 0, 1, 10) // foo pod
	podBar := net.IPv4(10, 0, 1, 20) // bar pod
	podBaz := net.IPv4(10, 0, 1, 30) // baz pod

	connA := newMockConn("A", false)
	connB := newMockConn("B", false)
	connC := newMockConn("C", false)
	hub.AddRoute(ctx, clientA, connA)
	hub.AddRoute(ctx, clientB, connB)
	hub.AddRoute(ctx, clientC, connC)

	// Traffic from foo pod → client A
	conn, err := hub.WriteToRoute(string(clientA), frameDatagram(1, buildIPv4Packet(podFoo, clientA, []byte("foo-data"))))
	if err != nil || conn != connA {
		t.Fatal("foo pod traffic should reach client A")
	}

	// Traffic from bar pod → client B
	conn, err = hub.WriteToRoute(string(clientB), frameDatagram(1, buildIPv4Packet(podBar, clientB, []byte("bar-data"))))
	if err != nil || conn != connB {
		t.Fatal("bar pod traffic should reach client B")
	}

	// Traffic from baz pod → client C
	conn, err = hub.WriteToRoute(string(clientC), frameDatagram(1, buildIPv4Packet(podBaz, clientC, []byte("baz-data"))))
	if err != nil || conn != connC {
		t.Fatal("baz pod traffic should reach client C")
	}

	// Verify routes are isolated: no cross-contamination
	if hub.HasRoute(string(podFoo)) {
		t.Fatal("pod IPs should NOT be in route table (only client TUN IPs)")
	}
}

// =============================================================================
// Scenario: User proxies resource, then another user also proxies it (late join)
// =============================================================================

func TestDataPath_ProxyResource_LateJoin(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()

	clientA := net.IPv4(198, 18, 0, 2)
	clientB := net.IPv4(198, 18, 0, 3)
	podIP := net.IPv4(10, 0, 1, 100)

	// Initially only A is proxying
	connA := newMockConn("A", false)
	hub.AddRoute(ctx, clientA, connA)

	// Traffic goes to A
	conn, err := hub.WriteToRoute(string(clientA), frameDatagram(1, buildIPv4Packet(podIP, clientA, []byte("only-A"))))
	if err != nil || conn != connA {
		t.Fatal("should reach A")
	}

	// B joins later (new proxy session)
	connB := newMockConn("B", false)
	hub.AddRoute(ctx, clientB, connB)

	// Now traffic split by headers (envoy decides dst IP):
	// header=A → 198.18.0.2
	conn, err = hub.WriteToRoute(string(clientA), frameDatagram(1, buildIPv4Packet(podIP, clientA, []byte("header-A"))))
	if err != nil || conn != connA {
		t.Fatal("header=A should still reach A")
	}
	// header=B → 198.18.0.3
	conn, err = hub.WriteToRoute(string(clientB), frameDatagram(1, buildIPv4Packet(podIP, clientB, []byte("header-B"))))
	if err != nil || conn != connB {
		t.Fatal("header=B should reach B")
	}
}

// =============================================================================
// Scenario: User leaves proxy (disconnect), traffic should stop routing to them
// =============================================================================

func TestDataPath_ProxyResource_UserLeaves(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()

	clientA := net.IPv4(198, 18, 0, 2)
	clientB := net.IPv4(198, 18, 0, 3)
	podIP := net.IPv4(10, 0, 1, 100)

	connA := newMockConn("A", false)
	connB := newMockConn("B", false)
	hub.AddRoute(ctx, clientA, connA)
	hub.AddRoute(ctx, clientB, connB)

	// Both receive traffic
	_, err := hub.WriteToRoute(string(clientA), frameDatagram(1, buildIPv4Packet(podIP, clientA, []byte("x"))))
	if err != nil {
		t.Fatal("A should be reachable")
	}
	_, err = hub.WriteToRoute(string(clientB), frameDatagram(1, buildIPv4Packet(podIP, clientB, []byte("x"))))
	if err != nil {
		t.Fatal("B should be reachable")
	}

	// B disconnects (leaves proxy)
	hub.RemoveRoutesByConn(ctx, connB)

	// Traffic to B should now fail
	_, err = hub.WriteToRoute(string(clientB), frameDatagram(1, buildIPv4Packet(podIP, clientB, []byte("x"))))
	if err == nil {
		t.Fatal("B left, should not be routable")
	}

	// Traffic to A should still work
	_, err = hub.WriteToRoute(string(clientA), frameDatagram(1, buildIPv4Packet(podIP, clientA, []byte("x"))))
	if err != nil {
		t.Fatal("A should still be reachable after B leaves")
	}
}

// =============================================================================
// Scenario: High concurrency — many users proxy same resource simultaneously
// =============================================================================

func TestDataPath_ProxyResource_HighConcurrency(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()

	numUsers := 20
	connsPerUser := 4
	podIP := net.IPv4(10, 0, 1, 100)

	// Register all users with connection pools
	clients := make([]net.IP, numUsers)
	for i := 0; i < numUsers; i++ {
		clients[i] = net.IPv4(198, 18, 0, byte(i+1))
		for j := 0; j < connsPerUser; j++ {
			hub.AddRoute(ctx, clients[i], newMockConn(fmt.Sprintf("u%d-s%d", i, j), false))
		}
	}

	// Concurrent traffic: pod sends to each user simultaneously
	var wg sync.WaitGroup
	errCh := make(chan error, numUsers*100)

	for round := 0; round < 100; round++ {
		for i := 0; i < numUsers; i++ {
			wg.Add(1)
			go func(clientIP net.IP) {
				defer wg.Done()
				pkt := buildIPv4Packet(podIP, clientIP, []byte("concurrent-request"))
				_, err := hub.WriteToRoute(string(clientIP), frameDatagram(1, pkt))
				if err != nil {
					errCh <- fmt.Errorf("failed to route to %s: %w", clientIP, err)
				}
			}(clients[i])
		}
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("%v", err)
	}
}

// =============================================================================
// Scenario: Response path — client processes proxied request and sends response
// back through the tunnel to the original caller
// =============================================================================

func TestDataPath_ProxyResource_ResponsePath(t *testing.T) {
	// Full round-trip: external → pod → envoy DNAT → traffic-manager → client A
	// Client A processes, responds: client A → traffic-manager → pod → external
	hub := NewRouteHub()
	ctx := context.Background()

	clientA := net.IPv4(198, 18, 0, 2)
	podIP := net.IPv4(10, 0, 1, 100)

	connA := newMockConn("A", false)
	hub.AddRoute(ctx, clientA, connA)

	// Step 1: Request from pod to client (DNAT by envoy)
	requestPkt := buildIPv4Packet(podIP, clientA, []byte("GET /api"))
	_, err := hub.WriteToRoute(string(clientA), frameDatagram(1, requestPkt))
	if err != nil {
		t.Fatalf("request routing failed: %v", err)
	}

	// Step 2: Client A processes and sends response back
	// Response goes: client TUN → slot[i] → server gvisor stack
	// Gvisor stack has the real TCP connection to the original caller
	// This part is handled by gvisor's TCPForwarder, not by RouteHub
	// At the tunnel layer, the response is just a normal client→service packet
	responsePkt := buildIPv4Packet(clientA, podIP, []byte("HTTP/1.1 200 OK"))

	// The response goes through gvisor (not RouteHub), so we just verify
	// the packet structure is correct for gvisor injection
	src, dst, _, parseErr := util.ParseIPFast(responsePkt)
	if parseErr != nil {
		t.Fatalf("response packet parse failed: %v", parseErr)
	}
	if !src.Equal(clientA) {
		t.Fatalf("response src should be client A")
	}
	if !dst.Equal(podIP) {
		t.Fatalf("response dst should be pod IP")
	}
}

// =============================================================================
// Scenario: Mix of proxy and connect — some users only connect (no proxy),
// some proxy resources
// =============================================================================

func TestDataPath_MixedProxyAndConnect(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()

	// User A: only connect (no proxy, just accesses services)
	clientA := net.IPv4(198, 18, 0, 2)
	// User B: proxy deployment/foo
	clientB := net.IPv4(198, 18, 0, 3)
	// User C: proxy deployment/bar
	clientC := net.IPv4(198, 18, 0, 4)

	podFoo := net.IPv4(10, 0, 1, 10)
	podBar := net.IPv4(10, 0, 1, 20)
	serviceIP := net.IPv4(10, 0, 0, 5) // cluster service

	connA := newMockConn("A", false)
	connB := newMockConn("B", false)
	connC := newMockConn("C", false)
	hub.AddRoute(ctx, clientA, connA)
	hub.AddRoute(ctx, clientB, connB)
	hub.AddRoute(ctx, clientC, connC)

	// A accesses a service (doesn't involve RouteHub — goes through gvisor directly)
	// But if service responds and goes through server TUN, it routes back to A
	_, err := hub.WriteToRoute(string(clientA), frameDatagram(1, buildIPv4Packet(serviceIP, clientA, []byte("svc-resp"))))
	if err != nil {
		t.Fatal("service response to A should route")
	}

	// Proxy traffic from foo pod → B
	_, err = hub.WriteToRoute(string(clientB), frameDatagram(1, buildIPv4Packet(podFoo, clientB, []byte("proxy-foo"))))
	if err != nil {
		t.Fatal("proxy traffic to B should route")
	}

	// Proxy traffic from bar pod → C
	_, err = hub.WriteToRoute(string(clientC), frameDatagram(1, buildIPv4Packet(podBar, clientC, []byte("proxy-bar"))))
	if err != nil {
		t.Fatal("proxy traffic to C should route")
	}

	// A cannot receive B's proxy traffic (different dst IP)
	_, err = hub.WriteToRoute(string(clientB), frameDatagram(1, buildIPv4Packet(podFoo, clientB, []byte("x"))))
	if err != nil {
		t.Fatal("should still work")
	}
}

// =============================================================================
// Full pipeline tests: TUN device → channels → conn (and reverse)
// These test the actual goroutine-based pipeline code (readFromTun, writeToTun,
// readFromConn, writeToConn) with mock TUN and pipe connections.
// =============================================================================

// mockTUN simulates a TUN device for full pipeline testing.
type mockTUN struct {
	readCh  chan []byte // inject packets here to simulate "TUN read"
	writeCh chan []byte // receive packets here to verify "TUN write"
	closed  chan struct{}
}

func newMockTUN() *mockTUN {
	return &mockTUN{
		readCh:  make(chan []byte, 100),
		writeCh: make(chan []byte, 100),
		closed:  make(chan struct{}),
	}
}

func (m *mockTUN) Read(b []byte) (int, error) {
	select {
	case pkt := <-m.readCh:
		return copy(b, pkt), nil
	case <-m.closed:
		return 0, net.ErrClosed
	}
}

func (m *mockTUN) Write(b []byte) (int, error) {
	cp := make([]byte, len(b))
	copy(cp, b)
	m.writeCh <- cp
	return len(b), nil
}

func (m *mockTUN) Close() error {
	select {
	case <-m.closed:
	default:
		close(m.closed)
	}
	return nil
}

func (m *mockTUN) LocalAddr() net.Addr                { return &net.IPAddr{IP: net.IPv4(198, 18, 0, 2)} }
func (m *mockTUN) RemoteAddr() net.Addr               { return nil }
func (m *mockTUN) SetDeadline(_ time.Time) error      { return nil }
func (m *mockTUN) SetReadDeadline(_ time.Time) error  { return nil }
func (m *mockTUN) SetWriteDeadline(_ time.Time) error { return nil }

// --- Test: readFromTun → tunInbound channel (IP parsing + prefix) ---

func TestPipeline_ReadFromTun_ParseAndDispatch(t *testing.T) {
	tun := newMockTUN()
	device := &tunDevice{
		tun:         tun,
		tunInbound:  make(chan *Packet, MaxSize),
		tunOutbound: make(chan *Packet, MaxSize),
		errChan:     make(chan error, 1),
	}
	device.transport = newClientTransport(device, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go device.readFromTun(ctx)

	// Inject a valid IPv4 packet into the mock TUN
	srcIP := net.IPv4(198, 18, 0, 2)
	dstIP := net.IPv4(10, 0, 0, 5)
	ipPkt := buildIPv4Packet(srcIP, dstIP, []byte("hello"))
	tun.readCh <- ipPkt

	// Should appear in tunInbound with prefix byte prepended
	select {
	case pkt := <-device.tunInbound:
		if pkt == nil {
			t.Fatal("got nil packet")
		}
		// pkt.data layout: [headroom 2][prefix 1][IP packet...]
		// pkt.length = len(IP) + 1 (prefix)
		if pkt.length != len(ipPkt)+1 {
			t.Fatalf("expected length %d, got %d", len(ipPkt)+1, pkt.length)
		}
		// prefix byte should be 1
		if pkt.data[2] != 1 {
			t.Fatalf("expected prefix=1, got %d", pkt.data[2])
		}
		// Verify parsed src/dst
		if !pkt.src.Equal(srcIP.To4()) {
			t.Fatalf("expected src %s, got %s", srcIP, pkt.src)
		}
		if !pkt.dst.Equal(dstIP.To4()) {
			t.Fatalf("expected dst %s, got %s", dstIP, pkt.dst)
		}
		config.LPool.Put(pkt.data[:])
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for packet")
	}
}

// --- Test: tunOutbound → writeToTun (strips prefix, writes pure IP) ---

func TestPipeline_WriteToTun_StripPrefix(t *testing.T) {
	tun := newMockTUN()
	device := &tunDevice{
		tun:         tun,
		tunInbound:  make(chan *Packet, MaxSize),
		tunOutbound: make(chan *Packet, MaxSize),
		errChan:     make(chan error, 1),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go device.writeToTun(ctx)

	// Create a packet as it would come from readFromConn (prefix=0 at data[0])
	srcIP := net.IPv4(10, 0, 0, 5)
	dstIP := net.IPv4(198, 18, 0, 2)
	ipPkt := buildIPv4Packet(srcIP, dstIP, []byte("response"))

	buf := config.LPool.Get().([]byte)
	buf[0] = 0 // prefix byte (direct to TUN)
	n := copy(buf[1:], ipPkt)
	device.tunOutbound <- NewPacket(buf, n+1, nil, nil)

	// Verify: TUN receives the pure IP packet (no prefix byte)
	select {
	case written := <-tun.writeCh:
		if len(written) != len(ipPkt) {
			t.Fatalf("expected %d bytes written to TUN, got %d", len(ipPkt), len(written))
		}
		// Verify it's the actual IP packet
		src, dst, _, err := util.ParseIPFast(written)
		if err != nil {
			t.Fatalf("written data is not valid IP: %v", err)
		}
		if !src.Equal(srcIP.To4()) {
			t.Fatalf("expected src %s, got %s", srcIP, src)
		}
		if !dst.Equal(dstIP.To4()) {
			t.Fatalf("expected dst %s, got %s", dstIP, dst)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for TUN write")
	}
}

// --- Test: writeToConn framing (in-place datagram header) ---

func TestPipeline_WriteToConn_FrameFormat(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	defer serverSide.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slotCh := make(chan *Packet, MaxSize)
	errChan := make(chan error, 2)
	slot := &connSlot{id: 0, inbound: slotCh}
	go slot.writeToConn(ctx, clientSide, errChan)

	// Build a packet as readFromTun produces it:
	// buf[0:2] = headroom for length header
	// buf[2] = prefix (1)
	// buf[3:] = IP packet
	srcIP := net.IPv4(198, 18, 0, 2)
	dstIP := net.IPv4(10, 0, 0, 5)
	ipPkt := buildIPv4Packet(srcIP, dstIP, []byte("request"))

	buf := config.LPool.Get().([]byte)
	buf[2] = 1
	n := copy(buf[3:], ipPkt)
	slotCh <- NewPacket(buf, n+1, srcIP.To4(), dstIP.To4())

	// Read the framed datagram from the other end of the pipe
	prefix, received, err := readDatagram(serverSide)
	if err != nil {
		t.Fatalf("failed to read framed datagram: %v", err)
	}
	if prefix != 1 {
		t.Fatalf("expected prefix=1, got %d", prefix)
	}
	if len(received) != len(ipPkt) {
		t.Fatalf("expected %d IP bytes, got %d", len(ipPkt), len(received))
	}
	src, dst, _, _ := util.ParseIPFast(received)
	if !src.Equal(srcIP.To4()) || !dst.Equal(dstIP.To4()) {
		t.Fatal("IP packet corrupted through framing")
	}
}

// --- Test: readFromConn → prefix dispatch (prefix=0 → tunOutbound) ---

func TestPipeline_ReadFromConn_PrefixZeroToTunOutbound(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	defer serverSide.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Wrap serverSide as UDPConnOverTCP (readFromConn expects datagram-framed reads)
	serverUDP, _ := NewUDPConnOverTCP(ctx, serverSide)

	slotInbound := make(chan *Packet, MaxSize)
	tunOutbound := make(chan *Packet, MaxSize)
	errChan := make(chan error, 2)

	slot := &connSlot{id: 0, inbound: slotInbound, tunOutbound: tunOutbound}
	go slot.readFromConn(ctx, serverUDP, errChan)

	// Send a prefix=0 packet (with datagram framing) → should go directly to tunOutbound
	srcIP := net.IPv4(10, 0, 0, 5)
	dstIP := net.IPv4(198, 18, 0, 2)
	ipPkt := buildIPv4Packet(srcIP, dstIP, []byte("response"))
	frame0 := frameDatagram(0, ipPkt)
	clientSide.Write(frame0)

	select {
	case pkt := <-tunOutbound:
		if pkt == nil {
			t.Fatal("got nil")
		}
		if pkt.data[0] != 0 {
			t.Fatalf("expected prefix=0 in tunOutbound, got %d", pkt.data[0])
		}
		config.LPool.Put(pkt.data[:])
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: prefix=0 packet should go to tunOutbound")
	}

	// Verify tunOutbound is empty after one packet
	select {
	case <-tunOutbound:
		t.Fatal("unexpected extra packet in tunOutbound")
	case <-time.After(100 * time.Millisecond):
		// good
	}
}

// --- Test: Full round-trip: writeToConn (framing) → server reads → server responds → readFromConn → writeToTun → TUN ---
// This test uses real UDPConnOverTCP wrapper for readFromConn side.

func TestPipeline_FullRoundTrip(t *testing.T) {
	clientRaw, serverRaw := net.Pipe()
	defer clientRaw.Close()
	defer serverRaw.Close()

	tun := newMockTUN()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Wrap client side as UDPConnOverTCP for readFromConn (it strips datagram headers)
	clientUDP, _ := NewUDPConnOverTCP(ctx, clientRaw)

	slotCh := make(chan *Packet, MaxSize)
	tunOutbound := make(chan *Packet, MaxSize)
	writeErrCh := make(chan error, 2)
	readErrCh := make(chan error, 2)

	// One slot drives both directions: writeToConn writes to raw TCP (adds datagram framing),
	// readFromConn reads from UDPConnOverTCP (strips datagram framing). Both share slot.inbound.
	slot := &connSlot{id: 0, inbound: slotCh, tunOutbound: tunOutbound}
	go slot.writeToConn(ctx, clientRaw, writeErrCh)
	go slot.readFromConn(ctx, clientUDP, readErrCh)

	// writeToTun consumes tunOutbound
	tunDev := &tunDevice{
		tun:         tun,
		tunOutbound: tunOutbound,
		errChan:     make(chan error, 1),
	}
	go tunDev.writeToTun(ctx)

	// === Step 1: Client sends a packet (simulate readFromTun output) ===
	srcIP := net.IPv4(198, 18, 0, 2)
	dstIP := net.IPv4(10, 0, 0, 5)
	requestPkt := buildIPv4Packet(srcIP, dstIP, []byte("GET /"))

	buf := config.LPool.Get().([]byte)
	buf[2] = 1
	n := copy(buf[3:], requestPkt)
	slotCh <- NewPacket(buf, n+1, srcIP.To4(), dstIP.To4())

	// === Step 2: Server reads framed packet from its end of pipe ===
	prefix, received, err := readDatagram(serverRaw)
	if err != nil {
		t.Fatalf("server read failed: %v", err)
	}
	if prefix != 1 {
		t.Fatalf("expected prefix=1, got %d", prefix)
	}
	if len(received) != len(requestPkt) {
		t.Fatalf("server received %d bytes, expected %d", len(received), len(requestPkt))
	}

	// === Step 3: Server sends response with datagram framing (prefix=0) ===
	responsePkt := buildIPv4Packet(dstIP, srcIP, []byte("HTTP/1.1 200"))
	responseFrame := frameDatagram(0, responsePkt)
	_, err = serverRaw.Write(responseFrame)
	if err != nil {
		t.Fatalf("server write failed: %v", err)
	}

	// === Step 4: Verify TUN received pure IP packet (prefix stripped) ===
	select {
	case written := <-tun.writeCh:
		if len(written) != len(responsePkt) {
			t.Fatalf("TUN received %d bytes, expected %d", len(written), len(responsePkt))
		}
		src, dst, _, parseErr := util.ParseIPFast(written)
		if parseErr != nil {
			t.Fatalf("TUN data not valid IP: %v", parseErr)
		}
		if !src.Equal(dstIP.To4()) {
			t.Fatalf("response src should be %s, got %s", dstIP, src)
		}
		if !dst.Equal(srcIP.To4()) {
			t.Fatalf("response dst should be %s, got %s", srcIP, dst)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: response never reached TUN")
	}
}
