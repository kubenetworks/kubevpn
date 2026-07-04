package core

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
)

func TestParseRoutes_ValidCIDRs(t *testing.T) {
	routes := parseRoutes("10.0.0.0/8,192.168.0.0/16")
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routes))
	}
	if routes[0].Dst.String() != "10.0.0.0/8" {
		t.Fatalf("expected 10.0.0.0/8, got %s", routes[0].Dst.String())
	}
	if routes[1].Dst.String() != "192.168.0.0/16" {
		t.Fatalf("expected 192.168.0.0/16, got %s", routes[1].Dst.String())
	}
}

func TestParseRoutes_WithSpaces(t *testing.T) {
	routes := parseRoutes(" 10.0.0.0/8 , 172.16.0.0/12 ")
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routes))
	}
}

func TestParseRoutes_Empty(t *testing.T) {
	routes := parseRoutes("")
	if len(routes) != 0 {
		t.Fatalf("expected 0 routes, got %d", len(routes))
	}
}

func TestParseRoutes_InvalidCIDR(t *testing.T) {
	routes := parseRoutes("invalid,10.0.0.0/8")
	if len(routes) != 1 {
		t.Fatalf("expected 1 valid route, got %d", len(routes))
	}
}

func TestParseForwarder_ValidRemote(t *testing.T) {
	fwd, err := ParseForwarder("tcp://127.0.0.1:8422")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fwd == nil {
		t.Fatal("expected non-nil forwarder")
	}
}

func TestParseForwarder_EmptyRemote(t *testing.T) {
	_, err := ParseForwarder("")
	if err == nil {
		t.Fatal("expected error for empty remote")
	}
}

func TestGenerateServers_UnknownProtocol(t *testing.T) {
	_, err := GenerateServers([]string{"unknown://:1234"}, nil)
	if err == nil {
		t.Fatal("expected error for unknown protocol")
	}
}

func TestGenerateServers_EmptyListeners(t *testing.T) {
	_, err := GenerateServers([]string{}, nil)
	if err == nil {
		t.Fatal("expected error for empty listeners")
	}
}

func TestGenerateServers_InvalidNode(t *testing.T) {
	_, err := GenerateServers([]string{""}, nil)
	if err == nil {
		t.Fatal("expected error for empty listener string")
	}
}

func TestGenerateServers_NilHub_UsesDefault(t *testing.T) {
	// Should not panic with nil hub
	_, _ = GenerateServers([]string{"unknown://:1234"}, nil)
}

func TestRegisterProtocol_CustomProtocol(t *testing.T) {
	called := false
	RegisterProtocol("test-proto", func(node *Node, hub *RouteHub) (net.Listener, Handler, error) {
		called = true
		return nil, nil, fmt.Errorf("test factory called")
	})
	defer delete(protocolRegistry, "test-proto")

	_, err := GenerateServers([]string{"test-proto://:9999"}, nil)
	if err == nil {
		t.Fatal("expected error from test factory")
	}
	if !called {
		t.Fatal("custom protocol factory was not called")
	}
}

func TestProtocolRegistry_AllProtocolsRegistered(t *testing.T) {
	expected := []string{"tun", "gtcp", "gudp", "ssh"}
	for _, proto := range expected {
		if _, ok := protocolRegistry[proto]; !ok {
			t.Fatalf("protocol %q not registered", proto)
		}
	}
}

// --- ConnList unit tests ---

func TestConnList_Add_Dedup(t *testing.T) {
	cl := &ConnList{}
	c1 := newMockConn("c1", false)
	c2 := newMockConn("c2", false)
	cl.Add(c1)
	cl.Add(c1)
	cl.Add(c2)
	if cl.Len() != 2 {
		t.Fatalf("expected 2 conns, got %d", cl.Len())
	}
}

func TestConnList_Remove(t *testing.T) {
	cl := &ConnList{}
	c1 := newMockConn("c1", false)
	c2 := newMockConn("c2", false)
	cl.Add(c1)
	cl.Add(c2)
	if empty := cl.Remove(c1); empty {
		t.Fatal("should not be empty")
	}
	if empty := cl.Remove(c2); !empty {
		t.Fatal("should be empty")
	}
}

func TestConnList_Write_Success(t *testing.T) {
	cl := &ConnList{}
	c1 := newMockConn("c1", false)
	cl.Add(c1)
	conn, err := cl.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conn != c1 {
		t.Fatal("expected c1")
	}
}

func TestConnList_Write_Fallback(t *testing.T) {
	cl := &ConnList{}
	dead := newMockConn("dead", true)
	alive := newMockConn("alive", false)
	cl.Add(dead)
	cl.Add(alive)
	conn, err := cl.Write([]byte("data"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conn != alive {
		t.Fatal("expected fallback to alive")
	}
	if cl.Len() != 1 {
		t.Fatalf("dead conn should be removed, got %d", cl.Len())
	}
}

func TestConnList_Write_AllDead(t *testing.T) {
	cl := &ConnList{}
	cl.Add(newMockConn("d1", true))
	cl.Add(newMockConn("d2", true))
	_, err := cl.Write([]byte("data"))
	if err == nil {
		t.Fatal("expected error")
	}
	if cl.Len() != 0 {
		t.Fatal("all dead conns should be removed")
	}
}

// --- RouteHub integration tests ---

func TestRouteHub_AddRoute_MultiConn(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()
	ip := net.ParseIP("198.18.0.5")
	hub.AddRoute(ctx, ip, newMockConn("c1", false))
	hub.AddRoute(ctx, ip, newMockConn("c2", false))
	val, _ := hub.RouteMapTCP.Load(string(ip))
	if val.(*ConnList).Len() != 2 {
		t.Fatalf("expected 2 conns")
	}
}

func TestRouteHub_WriteToRoute_Fallback(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()
	ip := net.ParseIP("198.18.0.10")
	dead := newMockConn("dead", true)
	alive := newMockConn("alive", false)
	hub.AddRoute(ctx, ip, dead)
	hub.AddRoute(ctx, ip, alive)
	conn, err := hub.WriteToRoute(string(ip), []byte("pkt"))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if conn != alive {
		t.Fatal("expected alive")
	}
}

func TestRouteHub_WriteToRoute_AllDead_CleansUp(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()
	ip := net.ParseIP("198.18.0.20")
	hub.AddRoute(ctx, ip, newMockConn("d1", true))
	hub.AddRoute(ctx, ip, newMockConn("d2", true))
	_, err := hub.WriteToRoute(string(ip), []byte("x"))
	if err == nil {
		t.Fatal("expected error")
	}
	if hub.HasRoute(string(ip)) {
		t.Fatal("empty entry should be deleted")
	}
}

func TestRouteHub_RemoveRoutesByConn(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()
	ip1 := net.ParseIP("198.18.0.30")
	ip2 := net.ParseIP("198.18.0.31")
	shared := newMockConn("shared", false)
	other := newMockConn("other", false)
	hub.AddRoute(ctx, ip1, shared)
	hub.AddRoute(ctx, ip1, other)
	hub.AddRoute(ctx, ip2, shared)
	hub.RemoveRoutesByConn(ctx, shared)
	if !hub.HasRoute(string(ip1)) {
		t.Fatal("ip1 should still have 'other'")
	}
	if hub.HasRoute(string(ip2)) {
		t.Fatal("ip2 should be removed (only had shared)")
	}
}

// --- End-to-end scenario tests ---

func TestScenario_SlotReconnect_NoPacketLoss(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()
	clientIP := net.ParseIP("198.18.0.100")
	key := string(clientIP)

	slot0 := newMockConn("slot0", false)
	slot1 := newMockConn("slot1", false)
	slot2 := newMockConn("slot2", false)
	slot3 := newMockConn("slot3", false)
	hub.AddRoute(ctx, clientIP, slot0)
	hub.AddRoute(ctx, clientIP, slot1)
	hub.AddRoute(ctx, clientIP, slot2)
	hub.AddRoute(ctx, clientIP, slot3)

	// slot0 and slot1 die (reconnecting)
	slot0.writeFail = true
	slot1.writeFail = true

	// Should fallback to slot2
	conn, err := hub.WriteToRoute(key, []byte("important"))
	if err != nil {
		t.Fatalf("packet lost: %v", err)
	}
	if conn != slot2 {
		t.Fatal("expected fallback to slot2")
	}

	// Dead slots auto-removed
	val, _ := hub.RouteMapTCP.Load(key)
	if val.(*ConnList).Len() != 2 {
		t.Fatal("expected 2 surviving")
	}

	// slot0 reconnects (new conn)
	newSlot0 := newMockConn("new0", false)
	hub.AddRoute(ctx, clientIP, newSlot0)
	if val.(*ConnList).Len() != 3 {
		t.Fatal("expected 3 after reconnect")
	}
}

func TestScenario_MultiClient_CrossTraffic(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()

	clientA := net.ParseIP("198.18.0.2")
	clientB := net.ParseIP("198.18.0.3")
	connA0 := newMockConn("A0", false)
	connA1 := newMockConn("A1", false)
	connB0 := newMockConn("B0", false)
	connB1 := newMockConn("B1", false)

	hub.AddRoute(ctx, clientA, connA0)
	hub.AddRoute(ctx, clientA, connA1)
	hub.AddRoute(ctx, clientB, connB0)
	hub.AddRoute(ctx, clientB, connB1)

	// A→B
	conn, err := hub.WriteToRoute(string(clientB), []byte("hi-B"))
	if err != nil {
		t.Fatalf("A→B failed: %v", err)
	}
	if conn != connB0 && conn != connB1 {
		t.Fatal("expected B's conn")
	}

	// B's slot0 dies, retry should use slot1
	connB0.writeFail = true
	conn, err = hub.WriteToRoute(string(clientB), []byte("retry"))
	if err != nil {
		t.Fatalf("fallback failed: %v", err)
	}
	if conn != connB1 {
		t.Fatal("expected B-slot1")
	}
}

func TestScenario_ConcurrentPoolRegistration(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()
	ip := net.ParseIP("198.18.0.50")

	var wg sync.WaitGroup
	conns := make([]*mockConn, 4)
	for i := range conns {
		conns[i] = newMockConn(fmt.Sprintf("c%d", i), false)
		wg.Add(1)
		go func(c *mockConn) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				hub.AddRoute(ctx, ip, c)
			}
		}(conns[i])
	}
	wg.Wait()

	val, _ := hub.RouteMapTCP.Load(string(ip))
	if val.(*ConnList).Len() != 4 {
		t.Fatalf("expected 4, got %d", val.(*ConnList).Len())
	}

	// Concurrent writes
	wg = sync.WaitGroup{}
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := hub.WriteToRoute(string(ip), []byte("data"))
			if err != nil {
				t.Errorf("concurrent write failed: %v", err)
			}
		}()
	}
	wg.Wait()
}
