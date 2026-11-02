package tunnel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestFramingRouteCallbackFailureKeepsSocket(t *testing.T) {
	for _, useHub := range []bool{false, true} {
		t.Run(fmt.Sprintf("hub=%v", useHub), func(t *testing.T) {
			raw, peer := batchTCPPair(t, false)
			tracked := &framingConn{Conn: raw}
			fallbackRaw, _ := batchTCPPair(t, false)
			hub := NewRouter()
			ip := net.ParseIP("198.18.0.92").To4()
			hub.AddRoute(context.Background(), ip, fallbackRaw)
			hub.AddRoute(context.Background(), ip, tracked)
			cl := &ConnList{}
			cl.Add(fallbackRaw)
			cl.Add(tracked)
			callback := func(c net.Conn) error {
				if c == tracked {
					return context.Canceled
				}
				return nil
			}
			var used net.Conn
			var err error
			if useHub {
				used, err = hub.WriteFuncToRoute(string(ip), callback)
			} else {
				used, err = cl.WriteFunc(callback)
			}
			if err != nil || used != fallbackRaw || tracked.closes.Load() != 0 {
				t.Fatalf("callback changed transport semantics: used=%v err=%v closes=%d", used, err, tracked.closes.Load())
			}
			if _, err = tracked.Write([]byte{9}); err != nil {
				t.Fatal(err)
			}
			_ = peer.SetReadDeadline(time.Now().Add(time.Second))
			var data [1]byte
			if _, err = io.ReadFull(peer, data[:]); err != nil || data[0] != 9 {
				t.Fatalf("callback damaged live transport: %v", err)
			}
		})
	}
}

// The bytes reach the real peer before cancellation, but Write does not return
// until the test has observed Close and installed replacement memberships.
type framingWriteReturnBarrier struct {
	*framingConn
	written, release, closed chan struct{}
}

func (c *framingWriteReturnBarrier) Write(b []byte) (int, error) {
	n, err := c.framingConn.Write(b)
	close(c.written)
	<-c.release
	return n, err
}

func (c *framingWriteReturnBarrier) Close() error {
	err := c.framingConn.Close()
	close(c.closed)
	return err
}

func TestFramingRouteFullWriteCancellationReconcilesBothKeys(t *testing.T) {
	raw, peer := batchTCPPair(t, false)
	c := &framingWriteReturnBarrier{framingConn: &framingConn{Conn: raw}, written: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(c.release) })
	hub := NewRouter()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ips := []net.IP{net.ParseIP("198.18.0.94").To4(), net.ParseIP("fd00::94")}
	fallbackRaw, _ := batchTCPPair(t, false)
	fallback := &framingConn{Conn: fallbackRaw}
	for _, ip := range ips {
		hub.AddRoute(ctx, ip, fallback)
		hub.AddRoute(ctx, ip, c)
	}
	buf := config.LPool.Get().([]byte)
	copy(buf, []byte{0, 1, 9})
	pkt := newPacket(buf, 1, nil, nil)
	defer pkt.release()
	done := make(chan struct{})
	var result error
	go func() { defer close(done); _, result = hub.WriteToRoutePacket(ctx, string(ips[0]), pkt) }()
	framingWait(t, c.written)
	cancel()
	framingWait(t, c.closed)
	replacementRaw, _ := batchTCPPair(t, false)
	replacement := &framingConn{Conn: replacementRaw}
	for _, ip := range ips {
		hub.AddRoute(context.Background(), ip, replacement)
	}
	release.Do(func() { close(c.release) })
	framingWait(t, done)
	if !errors.Is(result, context.Canceled) {
		t.Fatalf("final error = %v", result)
	}
	for _, ip := range ips {
		members := routeSnapshot(hub)[string(ip)]
		if len(members) != 2 || members[0] != replacement || members[1] != fallback {
			t.Errorf("stale original or lost replacement for %s: %v", ip, members)
		}
	}
	if c.closes.Load() != 1 || c.writes.Load() != 1 || fallback.writes.Load() != 0 || replacement.writes.Load() != 0 {
		t.Fatal("duplicate Close, I/O reuse, or frame replay")
	}
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	wire, err := io.ReadAll(peer)
	if err != nil || !bytes.Equal(wire, []byte{0, 1, 9}) {
		t.Fatalf("wire=%x err=%v", wire, err)
	}
}

func TestFramingRouteFailurePreservesReplacement(t *testing.T) {
	raw, _ := batchTCPPair(t, false)
	replacement, _ := batchTCPPair(t, false)
	hub := NewRouter()
	ctx := context.Background()
	v4 := net.ParseIP("198.18.0.93").To4()
	v6 := net.ParseIP("fd00::93")
	tracked := &framingConn{Conn: raw, outcomes: []framingWriteResult{{1, io.ErrUnexpectedEOF}}}
	hub.AddRoute(ctx, v4, tracked)
	hub.AddRoute(ctx, v6, tracked)
	tracked.beforeClose = func() { hub.AddRoute(ctx, v4, replacement); hub.AddRoute(ctx, v6, replacement) }
	if _, err := hub.WriteToRoute(string(v4), []byte{0, 1, 9}); err != io.ErrUnexpectedEOF {
		t.Fatal(err)
	}
	for _, ip := range []net.IP{v4, v6} {
		members := routeSnapshot(hub)[string(ip)]
		if len(members) != 1 || members[0] != replacement {
			t.Fatal("late failure pruned replacement registration")
		}
	}
}

func TestFramingRouteFailureClosesSocket(t *testing.T) {
	for _, api := range []string{"bytes", "packet", "list-bytes", "list-packet"} {
		for _, progress := range []int{0, 1, 3} {
			t.Run(fmt.Sprintf("%s/progress=%d", api, progress), func(t *testing.T) {
				raw, peer := batchTCPPair(t, false)
				tracked := &framingConn{Conn: raw, outcomes: []framingWriteResult{{progress, io.ErrUnexpectedEOF}}}
				fallbackRaw, _ := batchTCPPair(t, false)
				fallback := &framingConn{Conn: fallbackRaw}
				hub := NewRouter()
				cl := &ConnList{}
				ctx := context.Background()
				v4 := net.ParseIP("198.18.0.91").To4()
				v6 := net.ParseIP("fd00::91")
				hub.AddRoute(ctx, v4, fallback)
				hub.AddRoute(ctx, v4, tracked)
				hub.AddRoute(ctx, v6, tracked)
				cl.Add(fallback)
				cl.Add(tracked)
				tracked.beforeClose = func() { hub.HasRoute(string(v4)); cl.Len() } // Close must not hold metadata locks.
				buf := config.LPool.Get().([]byte)
				copy(buf, []byte{0, 1, 9})
				pkt := newPacket(buf, 1, nil, nil)
				defer pkt.release()
				var err error
				switch api {
				case "bytes":
					_, err = hub.WriteToRoute(string(v4), buf[:3])
				case "packet":
					_, err = hub.WriteToRoutePacket(ctx, string(v4), pkt)
				case "list-bytes":
					_, err = cl.Write(buf[:3])
				case "list-packet":
					_, err = cl.WritePacket(ctx, pkt)
				}
				if progress > 0 && (err != io.ErrUnexpectedEOF || fallback.writes.Load() != 0) {
					t.Errorf("positive progress replayed: err=%v fallback=%d", err, fallback.writes.Load())
				}
				if tracked.closes.Load() != 1 {
					t.Error("failed raw socket not closed")
				}
				calls := tracked.writes.Load()
				if api == "bytes" || api == "packet" {
					if hub.HasRoute(string(v6)) {
						t.Error("closed socket retained IPv6 membership")
					}
					_, _ = hub.WriteToRoute(string(v6), buf[:3])
					_, _ = hub.WriteToRoute(string(v4), buf[:3])
				} else {
					_, _ = cl.Write(buf[:3])
				}
				if tracked.writes.Load() != calls {
					t.Error("subsequent I/O reused failed route socket")
				}
				_ = peer.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
				wire, readErr := io.ReadAll(peer)
				if readErr != nil || len(wire) != progress {
					t.Errorf("peer not closed: len=%d err=%v", len(wire), readErr)
				}
			})
		}
	}
}
