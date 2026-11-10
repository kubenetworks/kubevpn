package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

type floodWriteConn struct {
	net.Conn
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
	mu        sync.Mutex
	calls     int
	failAfter int
}

func (c *floodWriteConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.entered) })
	<-c.release
	c.mu.Lock()
	c.calls++
	calls := c.calls
	c.mu.Unlock()
	if c.failAfter > 0 && calls >= c.failAfter {
		return 0, io.ErrUnexpectedEOF
	}
	return c.Conn.Write(p)
}

// TestPacketOwnershipFloodIntegration pushes tracked packets through the real
// client transport queue, Router, bufferedTCP and packetBatch. Large valid IP
// packets force packetBatch's pending path in addition to its frame limit.
func TestPacketOwnershipFloodIntegration(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cancelAfter int
		fillQueue   bool
		failAfter   int
	}{
		{name: "complete-and-drain", fillQueue: true},
		{name: "cancel-first-admission", cancelAfter: 1},
		{name: "cancel-after-full-queue", cancelAfter: 3000, fillQueue: true},
		{name: "socket-error-and-drain", failAfter: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hub := NewRouter()
			raw, peer := net.Pipe()
			gated := &floodWriteConn{Conn: raw, entered: make(chan struct{}), release: make(chan struct{}), failAfter: tc.failAfter}
			buffered := newBufferedTCP(ctx, gated, func(conn *bufferedTCP) {
				hub.RemoveRoutesByConn(context.Background(), conn)
			})
			peerDone := make(chan struct{})
			go func() {
				_, _ = io.Copy(io.Discard, peer)
				close(peerDone)
			}()

			dev := &tunDevice{tunInbound: make(chan *packet, MaxSize)}
			transport := newClientTransport(dev, nil, nil)
			src := net.IPv4(198, 18, 0, 5).To4()
			dst := net.IPv4(10, 0, 0, 1).To4()
			if !hub.AddRoute(ctx, dst, buffered) {
				t.Fatal("failed to register flood route")
			}
			ip := buildIPv4Packet(src, dst, make([]byte, 2300-20))
			tracked := make([]*packet, 0, 4096)
			feedDone := make(chan struct{})
			var routeErrors int
			go func() {
				defer close(feedDone)
				for i := 0; i < 4096; i++ {
					packet := newPacketFromPool(config.LargeBufferSize)
					copy(packet.data[tunReserve:], ip)
					packet.length = len(ip) + typePrefixLen
					packet.src, packet.dst = src, dst
					tracked = append(tracked, packet)

					transport.routeOutbound(context.Background(), packet)
					packet = <-dev.tunInbound
					if ctx.Err() == nil {
						if _, err := hub.WriteToRoutePacket(ctx, string(dst), packet); err != nil {
							routeErrors++
						}
					}
					packet.release()
					if tc.cancelAfter > 0 && i+1 == tc.cancelAfter {
						cancel()
					}
				}
			}()

			if tc.fillQueue {
				select {
				case <-gated.entered:
				case <-time.After(3 * time.Second):
					t.Fatal("buffered writer did not start")
				}
				deadline := time.Now().Add(5 * time.Second)
				for len(buffered.ch) != MaxSize {
					if time.Now().After(deadline) {
						t.Fatal("buffered queue did not apply backpressure")
					}
					time.Sleep(time.Millisecond)
				}
			}
			close(gated.release)
			select {
			case <-feedDone:
			case <-time.After(15 * time.Second):
				t.Fatal("packet flood did not finish")
			}
			if tc.failAfter > 0 && routeErrors == 0 {
				t.Fatal("injected socket failure was not observed")
			}
			_ = buffered.Close()
			buffered.waitRun()
			_ = peer.Close()
			<-peerDone
			if len(dev.tunInbound) != 0 || len(buffered.ch) != 0 {
				t.Fatalf("queues not drained: device=%d buffered=%d", len(dev.tunInbound), len(buffered.ch))
			}
			if len(tracked) != 4096 {
				t.Fatalf("tracked packets=%d", len(tracked))
			}
			for i, packet := range tracked {
				if got := packet.refs.Load(); got != -1 {
					t.Fatalf("packet %d refs=%d after final release", i, got)
				}
			}
			if tc.cancelAfter > 0 && !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatal("cancellation point was not reached")
			}
		})
	}
}
