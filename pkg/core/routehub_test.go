package core

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

func TestNewRouteHub(t *testing.T) {
	hub := NewRouteHub()
	if hub == nil {
		t.Fatal("NewRouteHub returned nil")
	}
	if hub.RouteMapTCP == nil {
		t.Fatal("RouteMapTCP is nil")
	}
	if hub.TCPPacketChan == nil {
		t.Fatal("TCPPacketChan is nil")
	}
	if cap(hub.TCPPacketChan) != MaxSize {
		t.Fatalf("TCPPacketChan capacity = %d, want %d", cap(hub.TCPPacketChan), MaxSize)
	}
}

func TestNewRouteHub_NotShared(t *testing.T) {
	hub1 := NewRouteHub()
	hub2 := NewRouteHub()
	if hub1 == hub2 {
		t.Fatal("NewRouteHub should return distinct instances")
	}
}

func TestRouteHub_Isolation(t *testing.T) {
	hub1 := NewRouteHub()
	hub2 := NewRouteHub()
	ctx := context.Background()

	hub1.AddRoute(ctx, net.ParseIP("10.0.0.1"), newMockConn("10.0.0.1:8080", false))

	if hub2.HasRoute(string(net.ParseIP("10.0.0.1"))) {
		t.Fatal("hub2 should not see hub1's route entry")
	}
}

func TestRouteHub_SharedState(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()

	conn := newMockConn("192.168.1.1:8080", false)
	ip := net.ParseIP("198.18.0.100")
	hub.AddRoute(ctx, ip, conn)

	if !hub.HasRoute(string(ip)) {
		t.Fatal("expected to find route entry")
	}
}

func TestRouteHub_TCPPacketChan_ProduceConsume(t *testing.T) {
	hub := NewRouteHub()

	pkt := &Packet{
		data:   make([]byte, 100),
		length: 50,
		src:    net.ParseIP("198.18.0.100"),
		dst:    net.ParseIP("198.18.0.101"),
	}

	go func() {
		hub.TCPPacketChan <- pkt
	}()

	select {
	case received := <-hub.TCPPacketChan:
		if received != pkt {
			t.Fatal("received packet does not match sent packet")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for packet")
	}
}

func TestRouteHub_ConcurrentAccess(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ip := net.IPv4(198, 18, byte(i/256), byte(i%256))
			conn := newMockConn(ip.String()+":8080", false)
			hub.AddRoute(ctx, ip, conn)
		}(i)
	}

	wg.Wait()

	count := 0
	hub.RouteMapTCP.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 100 {
		t.Fatalf("expected 100 entries, got %d", count)
	}
}

func TestRouteHub_RouteMapTCP_DeleteEntry(t *testing.T) {
	hub := NewRouteHub()
	ctx := context.Background()

	conn := newMockConn("10.0.0.1:1234", false)
	ip := net.ParseIP("198.18.0.50")
	hub.AddRoute(ctx, ip, conn)
	hub.RemoveRoutesByConn(ctx, conn)

	if hub.HasRoute(string(ip)) {
		t.Fatal("entry should have been deleted")
	}
}

func TestTunHandler_NilHub_StoresNil(t *testing.T) {
	handler := TunHandler(nil, nil)
	th := handler.(*tunHandler)
	if th.hub != nil {
		t.Fatal("nil hub should be stored as nil, callers must provide explicit hub")
	}
}

func TestTunHandler_CustomHub(t *testing.T) {
	hub := NewRouteHub()
	handler := TunHandler(nil, hub)
	th := handler.(*tunHandler)
	if th.hub != hub {
		t.Fatal("handler should use the provided hub")
	}
}

func TestGvisorTCPHandler_NilHub_StoresNil(t *testing.T) {
	handler := GvisorTCPHandler(nil)
	gh := handler.(*gvisorTCPHandler)
	if gh.hub != nil {
		t.Fatal("nil hub should be stored as nil, callers must provide explicit hub")
	}
}

func TestGvisorTCPHandler_CustomHub(t *testing.T) {
	hub := NewRouteHub()
	handler := GvisorTCPHandler(hub)
	gh := handler.(*gvisorTCPHandler)
	if gh.hub != hub {
		t.Fatal("handler should use the provided hub")
	}
}

func TestGenerateServers_NilHub_PassedThrough(t *testing.T) {
	// nil hub is passed through to protocol factories; callers must provide explicit hub
	_, _ = GenerateServers([]string{"unknown://:0"}, nil)
}

// mockConn implements net.Conn for testing
type mockConn struct {
	net.Conn
	addr      string
	writeFail bool
}

func newMockConn(addr string, fail bool) *mockConn {
	return &mockConn{addr: addr, writeFail: fail}
}

func (m *mockConn) Write(b []byte) (int, error) {
	if m.writeFail {
		return 0, fmt.Errorf("connection dead")
	}
	return len(b), nil
}

func (m *mockConn) RemoteAddr() net.Addr {
	a, _ := net.ResolveTCPAddr("tcp", m.addr)
	return a
}

func (m *mockConn) LocalAddr() net.Addr {
	a, _ := net.ResolveTCPAddr("tcp", "127.0.0.1:0")
	return a
}
