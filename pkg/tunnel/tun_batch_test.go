package tunnel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// fakeBatchTUN implements batchTUN: it delivers one seeded batch of IP packets, then blocks
// on gate until the test cancels the context (mimicking a TUN with no further traffic).
type fakeBatchTUN struct {
	batch   int
	packets [][]byte
	gate    chan struct{}

	mu     sync.Mutex
	served bool
}

func (f *fakeBatchTUN) BatchSize() int { return f.batch }

func (f *fakeBatchTUN) ReadPackets(bufs [][]byte, sizes []int, off int) (int, error) {
	f.mu.Lock()
	served := f.served
	f.served = true
	f.mu.Unlock()
	if !served {
		n := 0
		for i, p := range f.packets {
			if i >= len(bufs) {
				break
			}
			copy(bufs[i][off:], p)
			sizes[i] = len(p)
			n++
		}
		return n, nil
	}
	<-f.gate
	return 0, context.Canceled
}

type writeBatchTUN struct {
	net.Conn
	batch   int
	calls   int
	written [][]byte
	short   bool
}

func (f *writeBatchTUN) BatchSize() int { return f.batch }
func (f *writeBatchTUN) WritePackets(packets [][]byte) (int, error) {
	f.calls++
	if len(packets) > f.batch {
		return 0, fmt.Errorf("batch exceeds device limit")
	}
	for _, p := range packets {
		f.written = append(f.written, append([]byte(nil), p...))
	}
	if f.short {
		return 0, nil
	}
	return len(packets), nil
}
func (f *writeBatchTUN) Write(p []byte) (int, error) {
	f.calls++
	f.written = append(f.written, append([]byte(nil), p...))
	return len(p), nil
}
func TestWriteTunBatch(t *testing.T) {
	for _, bs := range []int{1, 8, 64} {
		t.Run(fmt.Sprint(bs), func(t *testing.T) {
			f := &writeBatchTUN{batch: bs}
			q := make(chan *packet, 70)
			var packets []*packet
			for i := 0; i < 70; i++ {
				p := batchPacket(60, i)
				packets = append(packets, p)
				q <- p
			}
			close(q)
			d := &tunDevice{tun: f, tunOutbound: q, errChan: make(chan error, 1)}
			d.writeToTun(context.Background())
			limit := min(bs, 32)
			if f.calls != (70+limit-1)/limit {
				t.Fatalf("calls=%d", f.calls)
			}
			if len(f.written) != 70 {
				t.Fatalf("written=%d", len(f.written))
			}
			for i, p := range packets {
				if !bytes.Equal(f.written[i], p.data[3:60]) {
					t.Fatalf("packet %d corrupt", i)
				}
			}
			checkBatchRefs(t, packets)
		})
	}
}
func TestWriteTunBatchShortCount(t *testing.T) {
	f := &writeBatchTUN{batch: 8, short: true}
	q := make(chan *packet, 3)
	var packets []*packet
	for i := 0; i < 3; i++ {
		p := batchPacket(60, i)
		packets = append(packets, p)
		q <- p
	}
	close(q)
	d := &tunDevice{tun: f, tunOutbound: q, errChan: make(chan error, 1)}
	d.writeToTun(context.Background())
	select {
	case err := <-d.errChan:
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatal(err)
		}
	default:
		t.Fatal("short batch not reported")
	}
	if f.calls != 1 {
		t.Fatal("short packet write retried")
	}
	checkBatchRefs(t, packets)
}

// pumpBenchmarkSource returns ordinary, independently framed IP packets, not
// GRO-coalesced reads. It has one owner (the synchronous pump benchmark).
type pumpBenchmarkSource struct {
	batch, count, reads, limit int
	packet                     []byte
	cancel                     context.CancelFunc
}

func (f *pumpBenchmarkSource) BatchSize() int { return f.batch }
func (f *pumpBenchmarkSource) ReadPackets(bufs [][]byte, sizes []int, off int) (int, error) {
	if f.reads == f.limit {
		f.cancel()
		return 0, context.Canceled
	}
	f.reads++
	for i := 0; i < f.count; i++ {
		copy(bufs[i][off:], f.packet)
		sizes[i] = len(f.packet)
	}
	return f.count, nil
}

func BenchmarkPumpTunBatchReuse(b *testing.B) {
	for _, bs := range []int{1, 128} {
		for _, count := range []int{1, 32, 128} {
			if count > bs {
				continue
			}
			for _, size := range []int{64, config.DefaultMTU} {
				b.Run(fmt.Sprintf("batchSize=%d/packets=%d/size=%d", bs, count, size), func(b *testing.B) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					src, dst := net.IPv4(198, 18, 0, 2).To4(), net.IPv4(10, 0, 0, 1).To4()
					ip := buildIPv4Packet(src, dst, bytes.Repeat([]byte{0x5a}, size-20))
					f := &pumpBenchmarkSource{batch: bs, count: count, limit: b.N, packet: ip, cancel: cancel}
					d := &tunDevice{tunInbound: make(chan *packet, bs), errChan: make(chan error, 1)}
					ct := newClientTransport(d, nil, nil)
					defer drainPacketChan(d.tunInbound)
					delivered := 0
					dispatch := func(buf []byte, n int, s, dest net.IP) {
						ct.routeOutbound(ctx, buf, n, s, dest)
						p := <-d.tunInbound // actual packet construction, transfer and release
						valid := p.length == len(ip)+typePrefixLen && p.data[2] == packetTypeToGvisor &&
							p.src.Equal(src) && p.dst.Equal(dst) && bytes.Equal(p.data[tunReserve:datagramHeaderLen+p.length], ip)
						p.release()
						if !valid {
							b.Fatal("dispatched packet mismatch")
						}
						delivered++
					}
					b.ReportAllocs()
					b.SetBytes(int64(count * len(ip)))
					b.ResetTimer()
					d.pumpTunBatch(ctx, "benchmark", f, dispatch)
					b.StopTimer()
					if f.reads != b.N || delivered != b.N*count {
						b.Fatalf("reads=%d packets=%d", f.reads, delivered)
					}
					b.ReportMetric(float64(count), "packets/op")
					// Pool Get/Put are not injectable: do not infer borrow/return counts
					// from batch size or call them measured buffer reuse.
				})
			}
		}
	}
}

type stage4PumpSource struct {
	batch int
	read  func([][]byte, []int, int) (int, error)
}

func (f *stage4PumpSource) BatchSize() int { return f.batch }
func (f *stage4PumpSource) ReadPackets(b [][]byte, s []int, off int) (int, error) {
	return f.read(b, s, off)
}

func TestStage4PumpFrameBoundary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &tunDevice{tunInbound: make(chan *packet, 4), errChan: make(chan error, 1)}
	tr := newClientTransport(d, nil, nil)
	v4 := buildIPv4Packet(net.IPv4(198, 18, 0, 2), net.IPv4(10, 0, 0, 1), make([]byte, 65533-20))
	v6 := make([]byte, 40)
	v6[0] = 0x60
	copy(v6[8:24], net.ParseIP("2001:db8::1").To16())
	copy(v6[24:40], net.ParseIP("2001:db8::2").To16())
	reads := 0
	f := &stage4PumpSource{batch: 4, read: func(b [][]byte, s []int, off int) (int, error) {
		reads++
		if reads > 1 {
			cancel()
			return 0, context.Canceled
		}
		copy(b[0][off:], v4)
		s[0] = len(v4)
		copy(b[1][off:], v6)
		s[1] = len(v6)
		s[2] = 65534
		s[3] = 65535
		return 4, nil
	}}
	d.pumpTunBatch(ctx, "stage4", f, func(b []byte, n int, s, dst net.IP) { tr.routeOutbound(ctx, b, n, s, dst) })
	defer drainPacketChan(d.tunInbound)
	if len(d.tunInbound) != 2 {
		t.Fatalf("boundary dispatched=%d", len(d.tunInbound))
	}
	for _, want := range [][]byte{v4, v6} {
		p := <-d.tunInbound
		if p.length != len(want)+1 || !bytes.Equal(p.data[3:2+p.length], want) {
			t.Error("canonical frame mismatch")
		}
		p.release()
	}
}

func TestStage4PumpPositiveError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ip := buildIPv4Packet(net.IPv4(198, 18, 0, 2), net.IPv4(10, 0, 0, 1), []byte("reported prefix"))
	calls, delivered := 0, 0
	f := &stage4PumpSource{batch: 128}
	f.read = func(b [][]byte, s []int, off int) (int, error) {
		calls++
		if calls > 1 {
			cancel()
			return 0, context.Canceled
		}
		copy(b[0][off:], ip)
		s[0] = len(ip)
		return 1, io.ErrUnexpectedEOF
	}
	d := &tunDevice{errChan: make(chan error, 1)}
	d.pumpTunBatch(ctx, "stage4", f, func(b []byte, n int, _, _ net.IP) {
		defer config.LPool.Put(b)
		if !bytes.Equal(b[tunReserve:tunReserve+n], ip) {
			t.Error("corrupt prefix")
		}
		delivered++
	})
	if delivered != 1 {
		t.Fatalf("positive error discarded reported prefix: %d", delivered)
	}
}

// Counting is local to this pump, never a global pool replacement. Returning a
// short slice models packet.release returning a differently resliced buffer.
type stage4PumpPool struct {
	gets, puts int
	live       map[*byte]bool
}

func (p *stage4PumpPool) Get() any {
	p.gets++
	b := make([]byte, 7, config.LargeBufferSize+2)
	p.live[&b[0]] = true
	return b
}
func (p *stage4PumpPool) Put(v any) {
	b := v.([]byte)
	if !p.live[&b[0]] {
		panic("duplicate pool return")
	}
	delete(p.live, &b[0])
	p.puts++
}

func TestStage4PumpOwnership(t *testing.T) {
	for _, count := range []int{1, 128} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			d := &tunDevice{tunInbound: make(chan *packet, 512), errChan: make(chan error, 1)}
			// The private seam exercises exactly the production pump with a local pool.
			pump, ok := any(d).(interface {
				pumpTunBatchWithPool(context.Context, string, batchTUN, func([]byte, int, net.IP, net.IP), interface {
					Get() any
					Put(any)
				})
			})
			if !ok {
				t.Fatal("pump has no local ownership seam")
			}
			pool := &stage4PumpPool{live: make(map[*byte]bool)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tr := newClientTransport(d, nil, nil)
			var held []*packet
			var previous [][]byte
			reads := 0
			f := &stage4PumpSource{batch: 128}
			f.read = func(b [][]byte, s []int, off int) (int, error) {
				for i := range b {
					if len(b[i]) != config.LargeBufferSize {
						t.Fatalf("logical length=%d", len(b[i]))
					}
					if reads > 0 && (reads != 1 || i >= count) && &b[i][0] != &previous[i][0] {
						t.Fatalf("retained slot %d replaced at read %d", i, reads)
					}
					for _, p := range held {
						if &b[i][0] == &p.data[0] {
							t.Fatal("read overwrites held packet")
						}
					}
				}
				previous = append(previous[:0], b...)
				reads++
				switch reads {
				case 1:
					for i := 0; i < count; i++ {
						ip := buildIPv4Packet(net.IPv4(198, 18, 0, 2), net.IPv4(10, 0, 0, 1), []byte{byte(i)})
						copy(b[i][off:], ip)
						s[i] = len(ip)
					}
					return count, nil
				case 2:
					clear(b[0][off : off+20])
					s[0] = 20
					s[1] = 0
					s[2] = config.LargeBufferSize
					s[3] = -1
					return 4, nil
				case 3:
					return 0, nil
				case 4:
					return 0, io.ErrUnexpectedEOF
				default:
					cancel()
					return 0, context.Canceled
				}
			}
			pump.pumpTunBatchWithPool(ctx, "stage4", f, func(b []byte, n int, s, dst net.IP) {
				tr.routeOutbound(ctx, b, n, s, dst)
				held = append(held, <-d.tunInbound)
			}, pool)
			if reads != 5 || len(held) != count || pool.gets != 128+count || pool.puts != 128 {
				t.Fatalf("reads=%d held=%d get/put=%d/%d", reads, len(held), pool.gets, pool.puts)
			}
			t.Logf("batch=128 delivered=%d reads=%d measured Gets=%d pump Puts=%d held=%d", count, reads, pool.gets, pool.puts, len(held))
			for i, p := range held {
				if p.data[tunReserve+20] != byte(i) || p.length != 22 || p.data[2] != packetTypeToGvisor {
					t.Error("held frame changed")
				}
				// Account ownership before real packet release returns it to LPool.
				delete(pool.live, &p.data[0])
				p.release()
			}
			if len(pool.live) != 0 {
				t.Fatal("leaked buffers")
			}
		})
	}
}

// TestPumpTunBatchDispatchesEachSegment verifies that a single batched read yielding multiple
// packets (the GRO/segment-aware case) parses and dispatches each packet individually into the
// canonical [2 len][1 type][IP] layout, with the IP at buf[tunReserve:].
func TestPumpTunBatchDispatchesEachSegment(t *testing.T) {
	src := net.ParseIP("198.18.0.1")
	dst := net.ParseIP("10.0.0.1")
	p1 := buildIPv4Packet(src, dst, []byte("one"))
	p2 := buildIPv4Packet(src, dst, []byte("two"))
	p3 := buildIPv4Packet(src, dst, []byte("three"))

	fake := &fakeBatchTUN{batch: 8, packets: [][]byte{p1, p2, p3}, gate: make(chan struct{})}
	d := &tunDevice{errChan: make(chan error, 1)}

	var mu sync.Mutex
	var got [][]byte
	done := make(chan struct{})
	dispatch := func(buf []byte, n int, s, dd net.IP) {
		cp := make([]byte, n)
		copy(cp, buf[tunReserve:tunReserve+n])
		config.LPool.Put(buf[:]) // dispatch owns buf and returns it to the pool
		mu.Lock()
		got = append(got, cp)
		count := len(got)
		mu.Unlock()
		if count == 3 {
			close(done)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	go d.pumpTunBatch(ctx, "test", fake, dispatch)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for 3 dispatched packets")
	}
	cancel()
	close(fake.gate)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("dispatched %d packets, want 3", len(got))
	}
	for i, want := range [][]byte{p1, p2, p3} {
		if !bytes.Equal(got[i], want) {
			t.Errorf("packet %d mismatch:\n got  %v\n want %v", i, got[i], want)
		}
	}
}
