package tunnel

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// routeSnapshot copies membership under the same transaction lock as mutations.
// Tests never receive a live list or mutable route table.
func routeSnapshot(hub *Router) map[string][]net.Conn {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	result := make(map[string][]net.Conn, len(hub.routes))
	for key, list := range hub.routes {
		members := list.snapshot()
		for i := 0; i < members.len(); i++ {
			result[key] = append(result[key], members.at(i).conn)
		}
	}
	return result
}

func TestNewRouteHub(t *testing.T) {
	hub := NewRouter()
	if hub == nil {
		t.Fatal("NewRouter returned nil")
	}
	if routes := routeSnapshot(hub); routes == nil || len(routes) != 0 {
		t.Fatal("expected initialized, empty route table")
	}
	if hub.bridge.queue == nil {
		t.Fatal("tunQueue is nil")
	}
	if cap(hub.bridge.queue) != MaxSize {
		t.Fatalf("tunQueue capacity = %d, want %d", cap(hub.bridge.queue), MaxSize)
	}
}

func TestNewRouteHub_NotShared(t *testing.T) {
	hub1 := NewRouter()
	hub2 := NewRouter()
	if hub1 == hub2 {
		t.Fatal("NewRouter should return distinct instances")
	}
}

func TestRouteHub_Isolation(t *testing.T) {
	hub1 := NewRouter()
	hub2 := NewRouter()
	ctx := context.Background()

	hub1.AddRoute(ctx, net.ParseIP("10.0.0.1"), newMockConn("10.0.0.1:8080", false))

	if hub2.HasRoute(string(net.ParseIP("10.0.0.1"))) {
		t.Fatal("hub2 should not see hub1's route entry")
	}
}

func TestRouteHub_SharedState(t *testing.T) {
	hub := NewRouter()
	ctx := context.Background()

	conn := newMockConn("192.168.1.1:8080", false)
	ip := net.ParseIP("198.18.0.100")
	hub.AddRoute(ctx, ip, conn)

	if !hub.HasRoute(string(ip)) {
		t.Fatal("expected to find route entry")
	}
}

func TestRouteHub_tunQueue_ProduceConsume(t *testing.T) {
	hub := NewRouter()

	pkt := newPacket(config.LPool.Get().([]byte), 50, net.ParseIP("198.18.0.100"), net.ParseIP("198.18.0.101"))

	go func() {
		hub.bridge.send(context.Background(), pkt)
	}()

	select {
	case received := <-hub.bridge.queue:
		defer received.release()
		if received != pkt {
			t.Fatal("received packet does not match sent packet")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for packet")
	}
}

func TestRouteHub_ConcurrentAccess(t *testing.T) {
	hub := NewRouter()
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

	count := len(routeSnapshot(hub))
	if count != 100 {
		t.Fatalf("expected 100 entries, got %d", count)
	}
}

func TestRouteHub_RouteMapTCP_DeleteEntry(t *testing.T) {
	hub := NewRouter()
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
	handler := TunHandler(nil, nil, nil)
	th := handler.(*tunHandler)
	if th.hub != nil {
		t.Fatal("nil hub should be stored as nil, callers must provide explicit hub")
	}
}

func TestTunHandler_CustomHub(t *testing.T) {
	hub := NewRouter()
	handler := TunHandler(nil, hub, nil)
	th := handler.(*tunHandler)
	if th.hub != hub {
		t.Fatal("handler should use the provided hub")
	}
}

func TestGvisorTCPHandler_NilHub_OwnsRegistry(t *testing.T) {
	handler := GvisorTCPHandler(nil)
	gh := handler.(*gvisorTCPHandler)
	if gh.runtime == nil || gh.runtime.peers != gh.peerRegistry {
		t.Fatal("default router must own the handler's peer registry")
	}
	gh.Start(context.Background())
	if err := gh.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGvisorTCPHandler_CustomHub(t *testing.T) {
	hub := NewRouter()
	handler := GvisorTCPHandler(hub)
	gh := handler.(*gvisorTCPHandler)
	if gh.runtime != hub.serverRuntime {
		t.Fatal("handler should use the provided hub")
	}
}

// routeBenchmarkWriter transfers and releases the route-acquired reference
// synchronously, isolating membership/ownership cost from socket scheduling.
type routeBenchmarkWriter struct {
	*mockConn
	count int
}

func (w *routeBenchmarkWriter) writePacket(_ context.Context, p *packet) error {
	w.count++
	p.release()
	return nil
}

func BenchmarkRoutePacketSingleMember(b *testing.B) {
	for _, mode := range []string{"synchronous", "concreteTCP", "realTLS"} {
		for _, members := range []int{1, 2} {
			b.Run(fmt.Sprintf("%s/members=%d", mode, members), func(b *testing.B) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				hub := NewRouter()
				ip := net.IPv4(198, 18, 0, 3).To4()
				key := string(ip)
				p := batchPacket(1500, 7)
				p.release() // immutable fixture reference; route acquires on each write
				var writers []*routeBenchmarkWriter
				var sockets []net.Conn
				var workers sync.WaitGroup
				counts := make([]int, members)
				errs := make([]error, members)
				var buffered []*bufferedTCP
				defer func() {
					cancel()
					for _, c := range sockets {
						c.Close()
					}
					workers.Wait()
					if p.refs.Load() != 0 {
						b.Error("route retained packet")
					}
					p.release()
				}()
				for i := 0; i < members; i++ {
					if mode == "synchronous" {
						w := &routeBenchmarkWriter{mockConn: newMockConn(fmt.Sprintf("127.0.0.1:%d", i+1), false)}
						writers = append(writers, w)
						hub.AddRoute(ctx, ip, w)
						continue
					}
					raw, peer := batchTCPPair(b, mode == "realTLS")
					sockets = append(sockets, raw, peer)
					c := &bufferedTCP{runDone: make(chan struct{}), Conn: raw, ch: make(chan *packet, MaxSize), done: make(chan struct{})}
					buffered = append(buffered, c)
					hub.AddRoute(ctx, ip, c)
					workers.Add(2)
					go func() { defer workers.Done(); c.run(ctx) }()
					go func(i int) {
						defer workers.Done()
						counts[i], errs[i] = benchmarkFrameSink(peer, [][]byte{p.data[:1500]}, 0, nil)
					}(i)
				}
				b.ReportAllocs()
				b.SetBytes(1500)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := hub.WriteToRoutePacket(ctx, key, p); err != nil {
						b.Fatal(err)
					}
				}
				// End-to-end includes draining writers and validating the exact sink count.
				// No producer remains, so closing these test-owned queues is safe.
				for _, c := range buffered {
					close(c.ch)
				}
				workers.Wait()
				b.StopTimer()
				total := 0
				for _, w := range writers {
					total += w.count
				}
				for i, n := range counts {
					total += n
					if errs[i] != nil {
						b.Fatal(errs[i])
					}
				}
				if total != b.N {
					b.Fatalf("delivered %d frames want %d", total, b.N)
				}
			})
		}
	}
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

// Raw route failures now close their transport; do not delegate to the nil
// embedded Conn in this metadata-only fixture.
func (m *mockConn) Close() error { return nil }

func (m *mockConn) RemoteAddr() net.Addr {
	a, _ := net.ResolveTCPAddr("tcp", m.addr)
	return a
}

func (m *mockConn) LocalAddr() net.Addr {
	a, _ := net.ResolveTCPAddr("tcp", "127.0.0.1:0")
	return a
}
