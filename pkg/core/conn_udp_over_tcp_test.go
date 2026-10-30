package core

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// framingConn limits individual socket operations to expose fragmentation and
// records transport ownership without replacing the real TCP/TLS connection.
type framingWriteResult struct {
	n   int
	err error
}

type framingConn struct {
	net.Conn
	readLimit, writeLimit     int
	writeError                error
	zero                      bool
	reads, writes, closes     atomic.Int32
	readEntered, writeEntered chan struct{}
	closeError                error
	outcomes                  []framingWriteResult
	beforeClose               func()
}

func (c *framingConn) Read(b []byte) (int, error) {
	c.reads.Add(1)
	if c.readEntered != nil {
		select {
		case c.readEntered <- struct{}{}:
		default:
		}
	}
	if c.readLimit > 0 && len(b) > c.readLimit {
		b = b[:c.readLimit]
	}
	return c.Conn.Read(b)
}
func (c *framingConn) Write(b []byte) (int, error) {
	call := int(c.writes.Add(1)) - 1
	if call < len(c.outcomes) {
		result := c.outcomes[call]
		if result.n == 0 {
			return 0, result.err
		}
		n, err := c.Conn.Write(b[:min(result.n, len(b))])
		if err != nil {
			return n, err
		}
		return n, result.err
	}
	if c.writeEntered != nil {
		select {
		case c.writeEntered <- struct{}{}:
		default:
		}
	}
	if c.zero {
		return 0, nil
	}
	if c.writeLimit > 0 && len(b) > c.writeLimit {
		b = b[:c.writeLimit]
	}
	n, err := c.Conn.Write(b)
	if c.writeError != nil {
		return n, c.writeError
	}
	return n, err
}
func (c *framingConn) Close() error {
	c.closes.Add(1)
	if c.beforeClose != nil {
		c.beforeClose()
	}
	err := c.Conn.Close()
	if c.closeError != nil {
		return c.closeError
	}
	return err
}
func framingWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not finish")
	}
}
func framingNoPanicRead(t *testing.T, c net.Conn, b []byte) (n int, err error) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("framing read panicked: %v", p)
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return c.Read(b)
}

func TestFramingReadBoundaries(t *testing.T) {
	for _, tls := range []bool{false, true} {
		for _, size := range []int{0, 1, 65534, 65535} {
			t.Run(fmt.Sprintf("tls=%v/size=%d", tls, size), func(t *testing.T) {
				raw, peer := batchTCPPair(t, tls)
				_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
				c, _ := NewUDPConnOverTCP(context.Background(), &framingConn{Conn: raw, readLimit: 7})
				defer c.Close()
				payload := bytes.Repeat([]byte{0xab}, size)
				frame := make([]byte, 2+size)
				binary.BigEndian.PutUint16(frame, uint16(size))
				copy(frame[2:], payload)
				// Fragment the first header and coalesce its body with the next frame.
				done := make(chan struct{})
				go func() {
					defer close(done)
					_, _ = peer.Write(frame[:1])
					_, _ = peer.Write(append(frame[1:], 0, 1, 0xcd))
				}()
				buf := make([]byte, size)
				n, err := framingNoPanicRead(t, c, buf)
				if err != nil || n != size || !bytes.Equal(buf, payload) {
					t.Fatalf("read n=%d err=%v", n, err)
				}
				n, err = framingNoPanicRead(t, c, make([]byte, 1))
				if err != nil || n != 1 {
					t.Fatalf("next read n=%d err=%v", n, err)
				}
				framingWait(t, done)
			})
		}
	}
}

func TestFramingReadErrorsCloseStream(t *testing.T) {
	for _, tt := range []struct {
		name     string
		wire     []byte
		capacity int
		want     error
	}{
		{"nil", []byte{0, 1, 7}, 0, io.ErrShortBuffer},
		{"one", []byte{0, 2, 7, 8}, 1, io.ErrShortBuffer},
		{"slot", []byte{255, 255}, 65534, io.ErrShortBuffer},
		{"eof", nil, 8, io.EOF},
		{"header", []byte{0}, 8, io.ErrUnexpectedEOF},
		{"body", []byte{0, 3, 7}, 8, io.ErrUnexpectedEOF},
		{"emptyBody", []byte{0, 3}, 8, io.EOF},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, peer := batchTCPPair(t, false)
			tracked := &framingConn{Conn: raw}
			c, _ := NewUDPConnOverTCP(context.Background(), tracked)
			defer c.Close()
			_, _ = peer.Write(tt.wire)
			_ = peer.Close()
			_, err := framingNoPanicRead(t, c, make([]byte, tt.capacity))
			if err != tt.want {
				t.Errorf("got %v want exact %v", err, tt.want)
			}
			if tracked.closes.Load() != 1 {
				t.Errorf("framing error did not close transport")
			}
			before := tracked.reads.Load()
			_, err = c.Read(make([]byte, 8))
			if err == nil || tracked.reads.Load() != before {
				t.Error("damaged stream reused")
			}
		})
	}
}

func TestFramingWriteBoundaries(t *testing.T) {
	for _, tls := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls=%v", tls), func(t *testing.T) {
			raw, peer := batchTCPPair(t, tls)
			_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
			_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
			tracked := &framingConn{Conn: raw, writeLimit: 127}
			c, _ := NewUDPConnOverTCP(context.Background(), tracked)
			defer c.Close()
			if n, err := c.Write(make([]byte, 65536)); n != 0 || err == nil || tracked.writes.Load() != 0 {
				t.Fatalf("oversize n=%d err=%v writes=%d", n, err, tracked.writes.Load())
			}
			done := make(chan error, 1)
			sizes := []int{0, 1, 65534, 65535, 3}
			go func() {
				for _, size := range sizes {
					var hdr [2]byte
					if _, err := io.ReadFull(peer, hdr[:]); err != nil {
						done <- err
						return
					}
					if int(binary.BigEndian.Uint16(hdr[:])) != size {
						done <- fmt.Errorf("size mismatch: %d want %d", binary.BigEndian.Uint16(hdr[:]), size)
						return
					}
					b := make([]byte, size)
					if _, err := io.ReadFull(peer, b); err != nil {
						done <- err
						return
					}
					if !bytes.Equal(b, bytes.Repeat([]byte{byte(size)}, size)) {
						done <- errors.New("payload corrupted")
						return
					}
				}
				done <- nil
			}()
			for _, size := range sizes {
				if n, err := c.Write(bytes.Repeat([]byte{byte(size)}, size)); err != nil || n != size {
					t.Fatalf("size %d: n=%d err=%v", size, n, err)
				}
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFramingScriptedWriteFailures(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		for _, prefix := range []int{0, 1, 2, 4, 7} {
			for _, fault := range []error{nil, io.ErrUnexpectedEOF} {
				if prefix == 7 && fault == nil {
					continue
				}
				t.Run(fmt.Sprintf("tls=%v/prefix=%d/error=%v", encrypted, prefix, fault), func(t *testing.T) {
					raw, peer := batchTCPPair(t, encrypted)
					outcomes := []framingWriteResult{{0, fault}}
					if prefix > 0 {
						outcomes = append([]framingWriteResult{{prefix, nil}}, outcomes...)
					}
					if prefix == 2 || prefix == 7 {
						outcomes = []framingWriteResult{{prefix, fault}}
					}
					// Exact header success followed by zero progress is also covered.
					if prefix == 2 && fault == nil {
						outcomes = append(outcomes, framingWriteResult{})
					}
					tracked := &framingConn{Conn: raw, outcomes: outcomes}
					c, _ := NewUDPConnOverTCP(context.Background(), tracked)
					defer c.Close()
					n, err := c.Write([]byte("hello"))
					wantErr := fault
					if wantErr == nil {
						wantErr = io.ErrShortWrite
					}
					if n != max(prefix-2, 0) || err != wantErr {
						t.Errorf("write=%d/%v want %d/%v", n, err, max(prefix-2, 0), wantErr)
					}
					if tracked.closes.Load() != 1 {
						t.Error("failed transport not closed")
					}
					calls := tracked.writes.Load()
					if _, err = c.Write([]byte("next")); err == nil || tracked.writes.Load() != calls {
						t.Error("damaged stream reused")
					}
					_ = peer.SetReadDeadline(time.Now().Add(time.Second))
					wire, err := io.ReadAll(peer)
					if err != nil || !bytes.Equal(wire, []byte{0, 5, 'h', 'e', 'l', 'l', 'o'}[:prefix]) {
						t.Errorf("wire=%x err=%v", wire, err)
					}
				})
			}
		}
	}
}

func TestFramingRouteCallbackFailureKeepsSocket(t *testing.T) {
	for _, useHub := range []bool{false, true} {
		t.Run(fmt.Sprintf("hub=%v", useHub), func(t *testing.T) {
			raw, peer := batchTCPPair(t, false)
			tracked := &framingConn{Conn: raw}
			fallbackRaw, _ := batchTCPPair(t, false)
			hub := NewRouteHub()
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
	hub := NewRouteHub()
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
	pkt := NewPacket(buf, 1, nil, nil)
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
	hub := NewRouteHub()
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
				hub := NewRouteHub()
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
				pkt := NewPacket(buf, 1, nil, nil)
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

func TestFramingWriteProgress(t *testing.T) {
	for _, tls := range []bool{false, true} {
		for _, tt := range []struct {
			name    string
			limit   int
			zero    bool
			fault   error
			want    int
			wantErr error
		}{
			{"short", 1, false, nil, 5, nil},
			{"zero", 0, true, nil, 0, io.ErrShortWrite},
			{"header", 1, false, io.ErrUnexpectedEOF, 0, io.ErrUnexpectedEOF},
			{"body", 4, false, io.ErrUnexpectedEOF, 2, io.ErrUnexpectedEOF},
			{"full", 7, false, io.ErrUnexpectedEOF, 5, io.ErrUnexpectedEOF},
		} {
			t.Run(fmt.Sprintf("tls=%v/%s", tls, tt.name), func(t *testing.T) {
				raw, peer := batchTCPPair(t, tls)
				tracked := &framingConn{Conn: raw, writeLimit: tt.limit, zero: tt.zero, writeError: tt.fault}
				c, _ := NewUDPConnOverTCP(context.Background(), tracked)
				n, err := c.Write([]byte("hello"))
				if n != tt.want || err != tt.wantErr {
					t.Errorf("n=%d err=%v want %d/%v", n, err, tt.want, tt.wantErr)
				}
				if err != nil {
					if tracked.closes.Load() != 1 {
						t.Error("damaged transport not closed")
					}
					calls := tracked.writes.Load()
					if _, e := c.Write([]byte("later")); e == nil || tracked.writes.Load() != calls {
						t.Error("reused failed stream")
					}
				}
				_ = c.Close()
				_ = peer.SetReadDeadline(time.Now().Add(time.Second))
				wire, _ := io.ReadAll(peer)
				want := []byte{0, 5, 'h', 'e', 'l', 'l', 'o'}
				if tt.zero {
					want = nil
				} else if tt.fault != nil {
					want = want[:tt.limit]
				}
				if !bytes.Equal(wire, want) {
					t.Errorf("wire %x want %x", wire, want)
				}
			})
		}
	}
}

func TestFramingWriteDatagramValidation(t *testing.T) {
	for _, tt := range []struct {
		name    string
		size, n int
	}{{"negative", 8, -1}, {"overflow", 65538, 65536}, {"nil", 0, 0}, {"headroom", 1, 0}, {"payload", 4, 3}} {
		t.Run(tt.name, func(t *testing.T) {
			raw, _ := batchTCPPair(t, false)
			tracked := &framingConn{Conn: raw}
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("validation panicked: %v", p)
				}
			}()
			if err := writeDatagram(tracked, make([]byte, tt.size), tt.n); err == nil {
				t.Error("invalid frame accepted")
			}
			if tracked.writes.Load() != 0 {
				t.Error("invalid frame reached socket")
			}
		})
	}
	t.Run("short", func(t *testing.T) {
		raw, peer := batchTCPPair(t, false)
		if err := writeDatagram(&framingConn{Conn: raw, writeLimit: 1}, []byte{0, 0, 7, 8}, 2); err != nil {
			t.Fatal(err)
		}
		_ = raw.Close()
		b, _ := io.ReadAll(peer)
		if !bytes.Equal(b, []byte{0, 2, 7, 8}) {
			t.Fatalf("truncated frame %x", b)
		}
	})
}

func TestFramingCancelReadAndGate(t *testing.T) {
	for _, body := range []bool{false, true} {
		t.Run(fmt.Sprintf("body=%v", body), func(t *testing.T) {
			raw, peer := batchTCPPair(t, false)
			tracked := &framingConn{Conn: raw, readEntered: make(chan struct{}, 8)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c, _ := NewUDPConnOverTCP(ctx, tracked)
			defer c.Close()
			if body {
				_, _ = peer.Write([]byte{0, 8, 1})
			}
			done := make(chan struct{})
			go func() { defer close(done); _, _ = c.Read(make([]byte, 8)) }()
			framingWait(t, tracked.readEntered)
			if body {
				framingWait(t, tracked.readEntered)
			}
			queued := make(chan struct{})
			go func() { defer close(queued); _, _ = c.Read(make([]byte, 8)) }()
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				_ = raw.Close()
				t.Error("cancellation did not wake socket read")
			}
			framingWait(t, done)
			framingWait(t, queued)
			if tracked.closes.Load() != 1 {
				t.Error("cancellation must close actual transport once")
			}
		})
	}
}

func TestFramingCloseAndPreCanceled(t *testing.T) {
	for _, pre := range []bool{false, true} {
		t.Run(fmt.Sprintf("pre=%v", pre), func(t *testing.T) {
			raw, _ := batchTCPPair(t, false)
			want := errors.New("close result")
			tracked := &framingConn{Conn: raw, closeError: want}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if pre {
				cancel()
			}
			c, _ := NewUDPConnOverTCP(ctx, tracked)
			if pre {
				if _, err := c.Write([]byte("no")); err == nil || tracked.writes.Load() != 0 {
					t.Error("pre-canceled write reached socket")
				}
			}
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					if err := c.Close(); err != want {
						t.Errorf("close err=%v", err)
					}
				})
			}
			wg.Wait()
			cancel()
			if tracked.closes.Load() != 1 {
				t.Errorf("close calls=%d", tracked.closes.Load())
			}
		})
	}
}

func TestFramingConcurrentFullDuplex(t *testing.T) {
	for _, tls := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls=%v", tls), func(t *testing.T) {
			raw, peer := batchTCPPair(t, tls)
			_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
			_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
			a, _ := NewUDPConnOverTCP(context.Background(), &framingConn{Conn: raw, readLimit: 1, writeLimit: 1})
			defer a.Close()
			b, _ := NewUDPConnOverTCP(context.Background(), &framingConn{Conn: peer, readLimit: 1, writeLimit: 1})
			defer b.Close()
			var wg sync.WaitGroup
			for _, c := range []net.Conn{a, b} {
				seen := &sync.Map{}
				for i := range 12 {
					wg.Go(func() {
						p := bytes.Repeat([]byte{byte(i)}, 32)
						if n, err := c.Write(p); err != nil || n != len(p) {
							t.Errorf("write n=%d err=%v", n, err)
						}
					})
					wg.Go(func() {
						p := make([]byte, 32)
						n, err := c.Read(p)
						if err != nil || n != 32 || !bytes.Equal(p, bytes.Repeat(p[:1], 32)) {
							t.Errorf("corrupt concurrent frame n=%d err=%v data=%x", n, err, p)
							return
						}
						if _, dup := seen.LoadOrStore(p[0], true); dup {
							t.Error("duplicate frame")
						}
					})
				}
			}
			wg.Wait()
		})
	}
}

func framingSaturateSocket(t *testing.T, raw, peer net.Conn) {
	t.Helper()
	_ = raw.(*net.TCPConn).SetWriteBuffer(1024)
	_ = peer.(*net.TCPConn).SetReadBuffer(1024)
	// Saturate the actual socket before starting a framed write. The peer never
	// reads, so cancellation must close the transport rather than wait on a gate.
	_ = raw.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	for {
		if _, err := raw.Write(make([]byte, 65535)); err != nil {
			break
		}
	}
	_ = raw.SetWriteDeadline(time.Time{})
}

func TestFramingCancelBlockedWriteAndGate(t *testing.T) {
	raw, peer := batchTCPPair(t, false)
	framingSaturateSocket(t, raw, peer)
	tracked := &framingConn{Conn: raw, writeEntered: make(chan struct{}, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, _ := NewUDPConnOverTCP(ctx, tracked)
	defer c.Close()
	done := make(chan struct{})
	go func() { defer close(done); _, _ = c.Write(make([]byte, 65535)) }()
	framingWait(t, tracked.writeEntered)
	queued := make(chan struct{})
	go func() { defer close(queued); _, _ = c.Write([]byte("queued")) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		_ = raw.Close()
		t.Error("cancel did not wake blocked socket write")
	}
	framingWait(t, done)
	framingWait(t, queued)
	if tracked.closes.Load() != 1 {
		t.Error("transport not closed exactly once")
	}
}

func TestFramingDeadlinePreservesError(t *testing.T) {
	raw, _ := batchTCPPair(t, false)
	c, _ := NewUDPConnOverTCP(context.Background(), raw)
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	_, err := c.Read(make([]byte, 8))
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("timeout replaced by %v", err)
	}
	if _, err = c.Read(make([]byte, 8)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("stream reused after timeout: %v", err)
	}
}

// Regression: writeDatagram must be idempotent, because WriteFunc may call it more than
// once on the same buffer (retrying the next connection). It writes the length header in
// place into the reserved headroom, so repeated calls produce the identical frame.
func TestWriteDatagram_Idempotent(t *testing.T) {
	payload := []byte{0x45, 0xaa, 0xbb, 0xcc, 0xdd}
	data := make([]byte, 64)
	copy(data[datagramHeaderLen:], payload)

	var first, second bytes.Buffer
	if err := writeDatagram(&first, data, len(payload)); err != nil {
		t.Fatalf("first writeDatagram: %v", err)
	}
	if err := writeDatagram(&second, data, len(payload)); err != nil {
		t.Fatalf("second writeDatagram: %v", err)
	}

	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatalf("Write is not idempotent: first=%x second=%x", first.Bytes(), second.Bytes())
	}

	// Frame must be a 2-byte big-endian length prefix followed by the payload.
	want := append([]byte{0x00, byte(len(payload))}, payload...)
	if !bytes.Equal(first.Bytes(), want) {
		t.Fatalf("frame mismatch: got %x want %x", first.Bytes(), want)
	}
}
