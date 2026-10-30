package core

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"k8s.io/client-go/util/cert"
)

// batchTestConn instruments actual TCP/TLS writes, optionally injecting short writes.
type batchTestConn struct {
	net.Conn
	calls     int
	sizes     []int
	limit     int
	failAfter int
	zero      bool
	omitSizes bool
}

func (c *batchTestConn) Write(b []byte) (int, error) {
	c.calls++
	if !c.omitSizes {
		c.sizes = append(c.sizes, len(b))
	}
	if c.zero {
		return 0, nil
	}
	if c.limit > 0 && len(b) > c.limit {
		b = b[:c.limit]
	}
	n, err := c.Conn.Write(b)
	if c.failAfter > 0 && c.calls == c.failAfter {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}
func batchTCPPair(t testing.TB, encrypted bool) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	s, err := ln.Accept()
	if err != nil {
		c.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); s.Close() })
	if !encrypted {
		return c, s
	}
	cp, kp, err := cert.GenerateSelfSignedCertKeyWithFixtures("localhost", []net.IP{net.IPv4(127, 0, 0, 1)}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	key, err := tls.X509KeyPair(cp, kp)
	if err != nil {
		t.Fatal(err)
	}
	server := tls.Server(s, &tls.Config{Certificates: []tls.Certificate{key}, MinVersion: tls.VersionTLS13})
	client := tls.Client(c, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}) // test-only loopback certificate
	done := make(chan error, 1)
	go func() { done <- server.Handshake() }()
	if err := client.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return client, server
}
func batchPacket(size, seq int) *Packet {
	buf := config.LPool.Get().([]byte)
	if len(buf) < size {
		config.LPool.Put(buf)
		buf = make([]byte, size)
	}
	for i := 2; i < size; i++ {
		buf[i] = byte(seq)
	}
	binary.BigEndian.PutUint16(buf, uint16(size-2))
	p := NewPacket(buf, size-2, nil, nil)
	p.acquire()
	return p
}
func checkBatchRefs(t *testing.T, packets []*Packet) {
	t.Helper()
	for i, p := range packets {
		if got := p.refs.Load(); got != 0 {
			t.Errorf("packet %d extra refs=%d, want caller only", i, got)
		}
		p.release()
	}
}
func readBatchFrames(c net.Conn, sizes []int) error {
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for i, size := range sizes {
		var h [2]byte
		if _, err := io.ReadFull(c, h[:]); err != nil {
			return err
		}
		n := int(binary.BigEndian.Uint16(h[:]))
		if n != size-2 {
			return fmt.Errorf("frame %d size %d want %d", i, n, size-2)
		}
		data := make([]byte, n)
		if _, err := io.ReadFull(c, data); err != nil {
			return err
		}
		if !bytes.Equal(data, bytes.Repeat([]byte{byte(i)}, n)) {
			return fmt.Errorf("frame %d reordered or corrupted", i)
		}
	}
	return nil
}
func TestConnBatchFramedBurst(t *testing.T) {
	for _, writer := range []string{"slot", "buffered"} {
		for _, encrypted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/tls=%v", writer, encrypted), func(t *testing.T) {
				raw, peer := batchTCPPair(t, encrypted)
				conn := &batchTestConn{Conn: raw}
				// Byte-boundary pending, frame-count boundary, and maximum legal standalone frame.
				sizes := []int{40000, 30000, 100, 65537, 10}
				for range 65 {
					sizes = append(sizes, 30)
				}
				q := make(chan *Packet, len(sizes))
				var packets []*Packet
				for i, n := range sizes {
					p := batchPacket(n, i)
					packets = append(packets, p)
					q <- p
				}
				close(q)
				recv := make(chan error, 1)
				go func() { recv <- readBatchFrames(peer, sizes) }()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if writer == "slot" {
					s := &connSlot{inbound: q}
					s.writeToConn(ctx, conn, make(chan error, 1))
				} else {
					c := &bufferedTCP{Conn: conn, ch: q, done: make(chan struct{})}
					c.run(ctx)
				}
				if err := <-recv; err != nil {
					t.Fatal(err)
				}
				checkBatchRefs(t, packets)
				if conn.calls != 6 {
					t.Fatalf("writes=%d want 6 bounded batches", conn.calls)
				}
				for _, n := range conn.sizes {
					if n > 65536 && n != 65537 {
						t.Errorf("unbounded write %d", n)
					}
				}
			})
		}
	}
}
func TestConnBatchTCPWritev(t *testing.T) {
	raw, peer := batchTCPPair(t, false)
	q := make(chan *Packet, 40)
	var packets []*Packet
	var sizes []int
	for i := 0; i < 40; i++ {
		p := batchPacket(1500, i)
		packets = append(packets, p)
		sizes = append(sizes, 1500)
		q <- p
	}
	close(q)
	recv := make(chan error, 1)
	go func() { recv <- readBatchFrames(peer, sizes) }()
	stats := &HeartbeatStats{}
	(&connSlot{inbound: q, stats: stats}).writeToConn(context.Background(), raw, make(chan error, 1))
	if err := <-recv; err != nil {
		t.Fatal(err)
	}
	if got := stats.Snapshot().Slots[0].WritePackets; got != 40 {
		t.Fatalf("per-frame stats %d", got)
	}
	checkBatchRefs(t, packets)
}

func TestConnBatchPartialFrameStats(t *testing.T) {
	raw, peer := batchTCPPair(t, false)
	conn := &batchTestConn{Conn: raw, limit: 25, failAfter: 1}
	q := make(chan *Packet, 3)
	var packets []*Packet
	for i := 0; i < 3; i++ {
		p := batchPacket(20, i)
		packets = append(packets, p)
		q <- p
	}
	close(q)
	stats := &HeartbeatStats{}
	(&connSlot{inbound: q, stats: stats}).writeToConn(context.Background(), conn, make(chan error, 1))
	raw.Close()
	data, err := io.ReadAll(peer)
	if err != nil || len(data) != 25 {
		t.Fatalf("partial write bytes %d: %v", len(data), err)
	}
	if got := stats.Snapshot().Slots[0].WritePackets; got != 1 {
		t.Fatalf("partial frame counted: %d", got)
	}
	checkBatchRefs(t, packets)
}

func TestConnBatchShortWrites(t *testing.T) {
	for _, writer := range []string{"slot", "buffered"} {
		t.Run(writer, func(t *testing.T) {
			raw, peer := batchTCPPair(t, false)
			conn := &batchTestConn{Conn: raw, limit: 7}
			q := make(chan *Packet, 3)
			var packets []*Packet
			sizes := []int{21, 40, 17}
			for i, n := range sizes {
				p := batchPacket(n, i)
				packets = append(packets, p)
				q <- p
			}
			close(q)
			recv := make(chan error, 1)
			go func() { recv <- readBatchFrames(peer, sizes) }()
			if writer == "slot" {
				s := &connSlot{inbound: q}
				s.writeToConn(context.Background(), conn, make(chan error, 1))
			} else {
				c := &bufferedTCP{Conn: conn, ch: q, done: make(chan struct{})}
				c.run(context.Background())
			}
			if err := <-recv; err != nil {
				t.Fatal(err)
			}
			checkBatchRefs(t, packets)
		})
	}
}
func TestConnBatchFailureOwnership(t *testing.T) {
	for _, zero := range []bool{false, true} {
		t.Run(fmt.Sprint(zero), func(t *testing.T) {
			raw, peer := batchTCPPair(t, false)
			conn := &batchTestConn{Conn: raw, limit: 7, failAfter: 1, zero: zero}
			q := make(chan *Packet, 4)
			var packets []*Packet
			for i, n := range []int{40000, 30000, 20, 20} {
				p := batchPacket(n, i)
				packets = append(packets, p)
				q <- p
			}
			recv := make(chan struct{})
			go func() { io.Copy(io.Discard, peer); close(recv) }()
			s := &connSlot{inbound: q}
			errs := make(chan error, 1)
			s.writeToConn(context.Background(), conn, errs)
			select {
			case err := <-errs:
				if zero && !errors.Is(err, io.ErrShortWrite) {
					t.Errorf("zero progress: %v", err)
				}
			default:
				t.Error("missing write error")
			}
			if conn.calls != 1 {
				t.Errorf("replayed failed write %d times", conn.calls)
			}
			// Current batch and local pending are released; untouched queue survives reconnect.
			if len(q) != 2 {
				t.Errorf("unused queue length=%d want 2", len(q))
			}
			drainPacketChan(q)
			checkBatchRefs(t, packets)
			raw.Close()
			<-recv
		})
	}
}
func TestConnBatchSparseImmediate(t *testing.T) {
	for _, writer := range []string{"slot", "buffered"} {
		t.Run(writer, func(t *testing.T) {
			raw, peer := batchTCPPair(t, false)
			q := make(chan *Packet, 1)
			p := batchPacket(20, 0)
			q <- p
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				if writer == "slot" {
					s := &connSlot{inbound: q}
					s.writeToConn(ctx, raw, make(chan error, 1))
				} else {
					c := &bufferedTCP{Conn: raw, ch: q, done: make(chan struct{})}
					c.run(ctx)
				}
			}()
			if err := readBatchFrames(peer, []int{20}); err != nil {
				t.Fatal(err)
			}
			cancel()
			raw.Close()
			<-done
			checkBatchRefs(t, []*Packet{p})
		})
	}
}

type batchBlockingConn struct {
	net.Conn
	entered chan struct{}
	once    sync.Once
}

func (c *batchBlockingConn) Write(b []byte) (int, error) {
	c.once.Do(func() { close(c.entered) })
	return c.Conn.Write(b)
}

// Hold cancellation callback dispatch until run has returned, deterministically
// exercising the idle path where stopClose wins before the callback can start.
type batchDeferredCancel struct {
	context.Context
	done       chan struct{}
	registered chan struct{}
	mu         sync.Mutex
	canceled   bool
	callback   func()
}

func (c *batchDeferredCancel) Done() <-chan struct{} { return c.done }
func (c *batchDeferredCancel) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.canceled {
		return context.Canceled
	}
	return nil
}
func (c *batchDeferredCancel) AfterFunc(fn func()) func() bool {
	c.mu.Lock()
	c.callback = fn
	c.mu.Unlock()
	close(c.registered)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		active := c.callback != nil
		c.callback = nil
		return active
	}
}
func (c *batchDeferredCancel) cancel() { c.mu.Lock(); c.canceled = true; close(c.done); c.mu.Unlock() }
func (c *batchDeferredCancel) dispatch() {
	c.mu.Lock()
	fn := c.callback
	c.callback = nil
	c.mu.Unlock()
	if fn != nil {
		fn()
	}
}

type batchCloseCountConn struct {
	net.Conn
	mu     sync.Mutex
	closes int
}

func (c *batchCloseCountConn) Close() error {
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()
	return c.Conn.Close()
}

func TestConnBatchBufferedIdleCancelClosesSocket(t *testing.T) {
	for _, mode := range []string{"precanceled", "idle-deferred-callback"} {
		t.Run(mode, func(t *testing.T) {
			raw, peer := net.Pipe()
			defer raw.Close()
			defer peer.Close()
			conn := &batchCloseCountConn{Conn: raw}
			var ctx context.Context
			var cancel context.CancelFunc
			if mode == "precanceled" {
				ctx, cancel = context.WithCancel(context.Background())
				cancel()
			} else {
				deferred := &batchDeferredCancel{Context: context.Background(), done: make(chan struct{}), registered: make(chan struct{})}
				ctx = deferred
				cancel = func() { lifetimeAwait(t, deferred.registered, "cancellation callback registration"); deferred.cancel() }
				defer deferred.dispatch()
			}
			c := &bufferedTCP{Conn: conn, ch: make(chan *Packet, 1), done: make(chan struct{})}
			done := make(chan struct{})
			go func() { c.run(ctx); close(done) }()
			if mode != "precanceled" {
				cancel()
			}
			lifetimeAwait(t, done, "idle buffered writer exit")
			_ = peer.SetReadDeadline(time.Now().Add(time.Second))
			var b [1]byte
			if _, err := peer.Read(b[:]); !errors.Is(err, io.EOF) {
				t.Fatalf("run returned without closing underlying socket: %v", err)
			}
			// Explicit owner cleanup and cancellation must share one socket close.
			c.Close()
			conn.mu.Lock()
			closes := conn.closes
			conn.mu.Unlock()
			if closes != 1 {
				t.Fatalf("underlying socket closed %d times, want 1", closes)
			}
		})
	}
}

func TestConnBatchBufferedBlockedWrite(t *testing.T) {
	for _, cancelOnly := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelOnly), func(t *testing.T) {
			raw, peer := net.Pipe()
			defer peer.Close()
			defer raw.Close()
			conn := &batchBlockingConn{Conn: raw, entered: make(chan struct{})}
			q := make(chan *Packet, 4)
			var packets []*Packet
			for i, n := range []int{40000, 30000, 20, 20} {
				p := batchPacket(n, i)
				packets = append(packets, p)
				q <- p
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := &bufferedTCP{Conn: conn, ch: q, done: make(chan struct{})}
			done := make(chan struct{})
			go func() { c.run(ctx); close(done) }()
			select {
			case <-conn.entered:
			case <-time.After(time.Second):
				t.Fatal("write never entered")
			}
			if cancelOnly {
				cancel()
			} else {
				c.Close()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("blocked write survived shutdown")
			}
			checkBatchRefs(t, packets)
		})
	}
}

func TestConnBatchBufferedWriteMaximum(t *testing.T) {
	raw, peer := batchTCPPair(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := NewBufferedTCP(ctx, raw)
	defer c.Close()
	p := batchPacket(65537, 0)
	if n, err := c.Write(p.data[:65537]); err != nil || n != 65537 {
		t.Fatalf("maximum frame write %d: %v", n, err)
	}
	if err := readBatchFrames(peer, []int{65537}); err != nil {
		t.Fatal(err)
	}
	p.release()
	p.release()
	if _, err := c.Write(make([]byte, 65538)); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

// Exercise the route API over actual TCP, including a short-write wrapper that
// reports progress and an error together. Neither byte nor packet routing may
// replay that prefix (or its suffix) on a fallback connection.
func TestStage3_RawRouteProgress(t *testing.T) {
	for _, api := range []string{"packet", "bytes", "list-packet", "list-bytes"} {
		for _, mode := range []string{"partial-error", "short", "zero"} {
			t.Run(api+"/"+mode, func(t *testing.T) {
				raw, peer := batchTCPPair(t, false)
				fallbackRaw, fallbackPeer := batchTCPPair(t, false)
				first := &batchTestConn{Conn: raw, limit: 3}
				fallback := &batchTestConn{Conn: fallbackRaw}
				if mode == "partial-error" {
					first.failAfter = 1
				}
				if mode == "zero" {
					first.zero = true
				}
				hub := NewRouteHub()
				ip := net.IPv4(198, 18, 0, 75).To4()
				hub.AddRoute(context.Background(), ip, fallback)
				hub.AddRoute(context.Background(), ip, first)
				hub.mu.Lock()
				cl := hub.routes[string(ip)]
				hub.mu.Unlock()
				p := batchPacket(20, 0)
				ctx, cancel := context.WithCancel(context.Background())
				var used net.Conn
				var err error
				switch api {
				case "packet":
					used, err = hub.WriteToRoutePacket(ctx, string(ip), p)
				case "bytes":
					used, err = hub.WriteToRoute(string(ip), p.data[:20])
				case "list-packet":
					used, err = cl.WritePacket(ctx, p)
				case "list-bytes":
					used, err = cl.Write(p.data[:20])
				}
				cancel() // successful writes must not leave a late socket-close callback
				p.release()
				defer checkBatchRefs(t, []*Packet{p})
				if mode == "partial-error" {
					if !errors.Is(err, io.ErrUnexpectedEOF) || used != nil || fallback.calls != 0 || first.calls != 1 {
						t.Fatalf("partial frame replay/lost error: used=%v err=%v calls=%d/%d", used, err, first.calls, fallback.calls)
					}
					if cl.Len() != 1 {
						t.Fatal("failed member was not retired")
					}
					prefix := make([]byte, 3)
					if _, err := io.ReadFull(peer, prefix); err != nil || !bytes.Equal(prefix, p.data[:3]) {
						t.Fatalf("wire prefix: %x %v", prefix, err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				reader := peer
				if mode == "zero" {
					reader = fallbackPeer
					if used != fallback || cl.Len() != 1 {
						t.Fatal("zero-progress failure did not fall back")
					}
				} else if used != first || first.calls != 7 {
					t.Fatal("short write did not continue on same socket")
				}
				got := make([]byte, 20)
				if _, err := io.ReadFull(reader, got); err != nil || !bytes.Equal(got, p.data[:20]) {
					t.Fatalf("frame mismatch: %x %v", got, err)
				}
				if _, err := used.Write([]byte{9}); err != nil {
					t.Fatalf("late cancellation closed completed write: %v", err)
				}
			})
		}
	}
}

func TestConnBatchBufferedCloseEnqueue(t *testing.T) {
	for _, cancelOnly := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelOnly), func(t *testing.T) {
			raw, peer := net.Pipe()
			defer peer.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := &bufferedTCP{Conn: raw, ch: make(chan *Packet, 8), done: make(chan struct{})}
			done := make(chan struct{})
			go func() { c.run(ctx); close(done) }()
			var packets []*Packet
			for i := 0; i < 64; i++ {
				packets = append(packets, batchPacket(20, i))
			}
			var wg sync.WaitGroup
			for _, p := range packets {
				wg.Add(1)
				go func(p *Packet) {
					defer wg.Done()
					if err := c.writePacket(ctx, p); err != nil {
						p.release()
					}
				}(p)
			}
			if cancelOnly {
				cancel()
			} else {
				c.Close()
			}
			joined := make(chan struct{})
			go func() { wg.Wait(); <-done; close(joined) }()
			select {
			case <-joined:
			case <-time.After(time.Second):
				raw.Close()
				<-joined
				t.Fatal("blocked writer not stopped")
			}
			checkBatchRefs(t, packets)
		})
	}
}
