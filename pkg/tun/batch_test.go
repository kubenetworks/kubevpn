package tun

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// fakeDevice is a minimal wireguard tun.Device: it serves pre-seeded read batches (each Read
// returns the next batch) and records everything written. It lets us exercise batchDevice's
// offset bridge, segment splitting, and boundary guard without a real TUN.
type fakeDevice struct {
	batch   int
	reads   [][][]byte // reads[i] is the batch of IP packets returned by the i-th Read
	readIdx int
	written [][]byte
}

func (f *fakeDevice) File() *os.File           { return nil }
func (f *fakeDevice) MTU() (int, error)        { return 1500, nil }
func (f *fakeDevice) Name() (string, error)    { return "faketun", nil }
func (f *fakeDevice) Events() <-chan tun.Event { return nil }
func (f *fakeDevice) Close() error             { return nil }

func (f *fakeDevice) BatchSize() int {
	if f.batch <= 0 {
		return 1
	}
	return f.batch
}

func (f *fakeDevice) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	if f.readIdx >= len(f.reads) {
		return 0, io.EOF
	}
	batch := f.reads[f.readIdx]
	f.readIdx++
	n := 0
	for i, p := range batch {
		if i >= len(bufs) {
			break
		}
		copy(bufs[i][offset:], p)
		sizes[i] = len(p)
		n++
	}
	return n, nil
}

func (f *fakeDevice) Write(bufs [][]byte, offset int) (int, error) {
	for _, b := range bufs {
		pkt := make([]byte, len(b)-offset)
		copy(pkt, b[offset:])
		f.written = append(f.written, pkt)
	}
	return len(bufs), nil
}

// eventLifecycleDevice models a native event producer whose blocking sends must
// finish before Close can stop it. abort is test-only failure cleanup; tests never
// receive from events, so only the real adapter can make the producer progress.
type eventLifecycleDevice struct {
	fakeDevice
	events       chan tun.Event
	producerDone chan struct{}
	stop         chan struct{}
	abort        chan struct{}
	stopOnce     sync.Once
	abortOnce    sync.Once
	eventsCalls  atomic.Int32
	closeCalls   atomic.Int32
	closeErr     error
}

func (f *eventLifecycleDevice) Events() <-chan tun.Event {
	f.eventsCalls.Add(1)
	return f.events
}

func (f *eventLifecycleDevice) Close() error {
	f.closeCalls.Add(1)
	f.stopOnce.Do(func() { close(f.stop) })
	<-f.producerDone
	return f.closeErr
}

func newEventLifecycleDevice(t *testing.T, mode string) *eventLifecycleDevice {
	t.Helper()
	f := &eventLifecycleDevice{
		events: make(chan tun.Event), producerDone: make(chan struct{}),
		stop: make(chan struct{}), abort: make(chan struct{}),
		closeErr: errors.New("native close sentinel"),
	}
	switch mode {
	case "nil":
		f.events = nil
		close(f.producerDone)
	case "closed":
		close(f.events)
		close(f.producerDone)
	default:
		go func() {
			defer close(f.producerDone)
			defer close(f.events)
			if mode == "on-close" || mode == "name" {
				select {
				case <-f.stop:
				case <-f.abort:
					return
				}
			}
			for i := 0; i < 32; i++ {
				select {
				case f.events <- tun.EventUp:
				case <-f.abort:
					return
				}
			}
		}()
	}
	t.Cleanup(func() {
		f.abortOnce.Do(func() { close(f.abort) })
		waitEventBarrier(t, f.producerDone, "producer cleanup")
	})
	return f
}

func waitEventBarrier(t *testing.T, done <-chan struct{}, phase string) bool {
	t.Helper()
	select {
	case <-done:
		return true
	case <-time.After(2 * time.Second):
		t.Errorf("%s exceeded event barrier budget", phase)
		return false
	}
}

func TestBatchDeviceEventLifecycle(t *testing.T) {
	for _, mode := range []string{"on-close", "early", "nil", "closed"} {
		t.Run(mode, func(t *testing.T) {
			f := newEventLifecycleDevice(t, mode)
			d := newBatchDevice(f)
			if got := f.eventsCalls.Load(); got != 1 {
				t.Errorf("Events called %d times at construction, want 1", got)
			}
			if mode == "nil" {
				select {
				case <-d.eventsDone:
				default:
					t.Error("nil Events must be joined at construction")
				}
			}
			if mode == "early" {
				waitEventBarrier(t, f.producerDone, "producer before Close")
			}
			if mode == "early" || mode == "closed" {
				waitEventBarrier(t, d.eventsDone, "event drainer before Close")
			}
			// Close must not acquire the IO locks, even when a read/write is blocked.
			d.rmu.Lock()
			d.wmu.Lock()
			var callers sync.WaitGroup
			results := make(chan error, 16)
			for i := 0; i < cap(results); i++ {
				callers.Add(1)
				go func() {
					defer callers.Done()
					results <- d.Close()
				}()
			}
			done := make(chan struct{})
			go func() { callers.Wait(); close(done) }()
			t.Cleanup(func() {
				f.abortOnce.Do(func() { close(f.abort) })
				waitEventBarrier(t, done, "Close callers cleanup")
				waitEventBarrier(t, d.eventsDone, "event drainer cleanup")
			})
			completed := waitEventBarrier(t, done, "concurrent Close")
			d.wmu.Unlock()
			d.rmu.Unlock()
			if !completed {
				// Release only the fake producer, never drain its events here.
				f.abortOnce.Do(func() { close(f.abort) })
				waitEventBarrier(t, f.producerDone, "aborted producer")
				waitEventBarrier(t, done, "aborted Close callers")
			}
			for i := 0; i < cap(results); i++ {
				select {
				case err := <-results:
					if err != f.closeErr {
						t.Errorf("Close error = %v, want same sentinel", err)
					}
				default:
					t.Error("Close caller did not return")
				}
			}
			if completed {
				select {
				case <-d.eventsDone:
				default:
					t.Error("Close returned before event drainer joined")
				}
				if err := d.Close(); err != f.closeErr {
					t.Errorf("repeated Close error = %v", err)
				}
			}
			if got := f.closeCalls.Load(); got != 1 {
				t.Errorf("native Close called %d times, want 1", got)
			}
			if got := f.eventsCalls.Load(); got != 1 {
				t.Errorf("Events called %d times, want 1", got)
			}
		})
	}
}

// nativeReadBenchmarkDevice writes into the supplied slices without append or
// replacement, like a native read. It does not emulate GRO on the read side.
type nativeReadBenchmarkDevice struct {
	fakeDevice
	count   int
	packets [][]byte
}

func (f *nativeReadBenchmarkDevice) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	for i := 0; i < f.count; i++ {
		copy(bufs[i][offset:], f.packets[i])
		sizes[i] = len(f.packets[i])
	}
	return f.count, nil
}

func poisonBatchReadDestination(dst [][]byte, sizes []int, size int) {
	clear(sizes)
	for _, buf := range dst {
		for i := range buf[3 : 3+size] {
			buf[3+i] = 0xff
		}
	}
}

func validateBatchReadDestination(dst [][]byte, sizes []int, packets [][]byte) error {
	for i, want := range packets {
		if sizes[i] != len(want) || !bytes.Equal(dst[i][3:3+len(want)], want) {
			return fmt.Errorf("packet %d mismatch", i)
		}
	}
	return nil
}

// Negative controls use the same poison/validator as the benchmark: neither
// successful warmup nor a correct returned count can stand in for measured IO.
func TestBatchReadBenchmarkRejectsStaleOutput(t *testing.T) {
	packets := [][]byte{bytes.Repeat([]byte{0}, 64), bytes.Repeat([]byte{1}, 64)}
	dev := &nativeReadBenchmarkDevice{fakeDevice: fakeDevice{batch: 2}, count: 2, packets: packets}
	d := newBatchDevice(dev)
	dst := [][]byte{make([]byte, 65536), make([]byte, 65536)}
	sizes := make([]int, 2)
	if n, err := d.ReadPackets(dst, sizes, 3); n != 2 || err != nil {
		t.Fatalf("warm read %d: %v", n, err)
	}
	if err := validateBatchReadDestination(dst, sizes, packets); err != nil {
		t.Fatal(err)
	}
	poisonBatchReadDestination(dst, sizes, 64)
	// A no-op after warmup returning the expected count leaves these poisoned
	// outputs untouched. Even fabricating the sizes must not pass validation.
	if err := validateBatchReadDestination(dst, sizes, packets); err == nil {
		t.Fatal("accepted no-op after warmup")
	}
	for i := range sizes {
		sizes[i] = 64
	}
	if err := validateBatchReadDestination(dst, sizes, packets); err == nil {
		t.Fatal("accepted poisoned payload with correct sizes")
	}
	if n, err := d.ReadPackets(dst, sizes, 3); n != 2 || err != nil {
		t.Fatalf("real read %d: %v", n, err)
	}
	if err := validateBatchReadDestination(dst, sizes, packets); err != nil {
		t.Fatal(err)
	}
	copy(dst[1][3:67], dst[0][3:67])
	if err := validateBatchReadDestination(dst, sizes, packets); err == nil {
		t.Fatal("accepted duplicated packet")
	}
}

func BenchmarkBatchDeviceReadPackets(b *testing.B) {
	b.Logf("%s: current ReadPackets is scratch-only; no direct algorithm comparison", runtime.GOOS)
	for _, bs := range []int{1, 128} {
		for _, count := range []int{1, 32, 128} {
			if count > bs {
				continue
			}
			for _, size := range []int{64, 1500} {
				for _, backing := range []int{65536, 65538} {
					b.Run(fmt.Sprintf("batchSize=%d/packets=%d/size=%d/logical=65536/backing=%d", bs, count, size, backing), func(b *testing.B) {
						packets := make([][]byte, count)
						for i := range packets {
							packets[i] = bytes.Repeat([]byte{byte(i)}, size)
						}
						dev := &nativeReadBenchmarkDevice{fakeDevice: fakeDevice{batch: bs}, count: count, packets: packets}
						d := newBatchDevice(dev)
						dst, sizes := make([][]byte, bs), make([]int, bs)
						for i := range dst {
							dst[i] = make([]byte, backing)[:65536:backing]
						}
						// Warm scratch allocation outside the timer. Keep this production
						// entrypoint unchanged when the real optimized algorithm arrives.
						if n, err := d.ReadPackets(dst, sizes, 3); err != nil || n != count {
							b.Fatalf("warm read %d: %v", n, err)
						}
						// Warmup output must not satisfy the post-measurement checks.
						poisonBatchReadDestination(dst, sizes, size)
						b.ReportAllocs()
						b.SetBytes(int64(count * size))
						b.ResetTimer()
						for i := 0; i < b.N; i++ {
							if n, err := d.ReadPackets(dst, sizes, 3); err != nil || n != count {
								b.Fatalf("read %d: %v", n, err)
							}
						}
						b.StopTimer()
						if err := validateBatchReadDestination(dst, sizes, packets); err != nil {
							b.Fatal(err)
						}
						b.ReportMetric(float64(count), "packets/op")
						// Explicit historical accounting, not an observed copy counter.
						b.ReportMetric(float64(count*size), "baseline-copy-B/op")
					})
				}
			}
		}
	}
}

// Algorithm model only: same fake packets and device copies for both paths,
// not Linux kernel/GSO throughput. The direct model includes the read lock but
// bypasses concrete-type/capacity qualification, which is tested separately.
func BenchmarkStage4ReadModel(b *testing.B) {
	for _, count := range []int{1, 32, 128} {
		for _, size := range []int{64, 1500} {
			for _, mode := range []string{"scratch", "direct"} {
				b.Run(fmt.Sprintf("%s/packets=%d/size=%d", mode, count, size), func(b *testing.B) {
					packets := make([][]byte, count)
					for i := range packets {
						packets[i] = bytes.Repeat([]byte{byte(i)}, size)
					}
					dev := &nativeReadBenchmarkDevice{fakeDevice: fakeDevice{batch: 128}, count: count, packets: packets}
					d := newBatchDevice(dev)
					dst, sizes := make([][]byte, 128), make([]int, 128)
					for i := range dst {
						dst[i] = make([]byte, 65536, 65538)
					}
					read := d.ReadPackets
					if mode == "direct" {
						read = func(dst [][]byte, sizes []int, off int) (int, error) {
							d.rmu.Lock()
							defer d.rmu.Unlock()
							return d.readPacketsDirect(dst, sizes, off)
						}
					}
					if n, e := read(dst, sizes, 3); n != count || e != nil {
						b.Fatal(n, e)
					}
					if e := validateBatchReadDestination(dst, sizes, packets); e != nil {
						b.Fatal(e)
					}
					poisonBatchReadDestination(dst, sizes, size)
					b.ReportAllocs()
					b.SetBytes(int64(count * size))
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						if n, e := read(dst, sizes, 3); n != count || e != nil {
							b.Fatal(n, e)
						}
					}
					b.StopTimer()
					if e := validateBatchReadDestination(dst, sizes, packets); e != nil {
						b.Fatal(e)
					}
					copyBytes := count * size
					if mode == "direct" {
						copyBytes = 0
					}
					b.ReportMetric(float64(copyBytes), "adapter-copy-B/op")
					b.ReportMetric(float64(count), "packets/op")
				})
			}
		}
	}
}

// TestBatchDeviceReadPacketsMulti verifies a single ReadPackets returns every packet of a
// batch (the GRO/segment-aware case), each copied into the caller's buffer at the offset.
func TestBatchDeviceReadPacketsMulti(t *testing.T) {
	p1 := []byte("packet-one")
	p2 := []byte("packet-two-is-longer")
	p3 := []byte("p3")
	bd := newBatchDevice(&fakeDevice{batch: 4, reads: [][][]byte{{p1, p2, p3}}})

	const off = 3
	dst := make([][]byte, bd.BatchSize())
	sizes := make([]int, bd.BatchSize())
	for i := range dst {
		dst[i] = make([]byte, 100)
	}

	n, err := bd.ReadPackets(dst, sizes, off)
	if err != nil {
		t.Fatalf("ReadPackets: %v", err)
	}
	if n != 3 {
		t.Fatalf("n = %d, want 3", n)
	}
	for i, want := range [][]byte{p1, p2, p3} {
		if got := dst[i][off : off+sizes[i]]; !bytes.Equal(got, want) {
			t.Errorf("packet %d = %q, want %q", i, got, want)
		}
	}
}

// TestBatchDeviceReadPacketsBoundaryGuard verifies a packet too large to fit the caller's
// buffer at the offset (a GRO-coalesced packet beyond the tunnel frame limit) is dropped,
// and the remaining packets are compacted.
func TestBatchDeviceReadPacketsBoundaryGuard(t *testing.T) {
	small := []byte("ok")
	big := make([]byte, 200) // 200 + off > dst buffer of 100 -> must be dropped
	for i := range big {
		big[i] = 'X'
	}
	bd := newBatchDevice(&fakeDevice{batch: 4, reads: [][][]byte{{small, big, small}}})

	const off = 3
	dst := make([][]byte, bd.BatchSize())
	sizes := make([]int, bd.BatchSize())
	for i := range dst {
		dst[i] = make([]byte, 100)
	}

	n, err := bd.ReadPackets(dst, sizes, off)
	if err != nil {
		t.Fatalf("ReadPackets: %v", err)
	}
	if n != 2 {
		t.Fatalf("n = %d, want 2 (oversized packet dropped)", n)
	}
	for i := 0; i < 2; i++ {
		if got := dst[i][off : off+sizes[i]]; !bytes.Equal(got, small) {
			t.Errorf("packet %d = %q, want %q", i, got, small)
		}
	}
}

// TestBatchDeviceWrite verifies the net.Conn write path hands the exact IP packet to the
// device (copied past the required header room, leaving the caller's slice untouched).
func TestBatchDeviceWrite(t *testing.T) {
	dev := &fakeDevice{batch: 1}
	bd := newBatchDevice(dev)

	pkt := []byte("hello-write-path")
	n, err := bd.Write(pkt)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(pkt) {
		t.Fatalf("n = %d, want %d", n, len(pkt))
	}
	if len(dev.written) != 1 || !bytes.Equal(dev.written[0], pkt) {
		t.Fatalf("written = %v, want [%q]", dev.written, pkt)
	}
}

// mutatingWriteDevice models wireguard's in-place headroom/GRO writes.
type mutatingWriteDevice struct {
	fakeDevice
	calls int
	count int
	err   error
}

func (f *mutatingWriteDevice) Write(bufs [][]byte, offset int) (int, error) {
	f.calls++
	if offset != 16 || len(bufs) > f.BatchSize() {
		return 0, fmt.Errorf("invalid batch/headroom: %d/%d", len(bufs), offset)
	}
	for _, b := range bufs {
		if cap(b) < offset+device.MaxSegmentSize {
			return 0, errors.New("insufficient GRO scratch")
		}
		if !bytes.Equal(b[:offset], make([]byte, offset)) {
			return 0, errors.New("poisoned write headroom not cleared")
		}
		f.written = append(f.written, append([]byte(nil), b[offset:]...))
		for i := range b {
			b[i] = 0xff
		}
	}
	if f.count >= 0 {
		return f.count, f.err
	}
	if runtime.GOOS == "linux" {
		total := 0
		for _, b := range bufs {
			total += len(b)
		}
		return total, f.err
	}
	return len(bufs), f.err
}
func TestBatchDeviceWritePackets(t *testing.T) {
	for _, bs := range []int{1, 4} {
		t.Run(fmt.Sprint(bs), func(t *testing.T) {
			dev := &mutatingWriteDevice{fakeDevice: fakeDevice{batch: bs}, count: -1}
			bd := newBatchDevice(dev)
			writer, ok := any(bd).(interface{ WritePackets([][]byte) (int, error) })
			if !ok {
				t.Fatal("missing WritePackets adapter")
			}
			var packets, original [][]byte
			for i := 0; i < 9; i++ {
				p := bytes.Repeat([]byte{byte(i)}, 60+i)
				p[0] = 0x45
				if i%2 == 1 {
					p[0] = 0x60
				}
				packets = append(packets, p)
				original = append(original, append([]byte(nil), p...))
			}
			n, err := writer.WritePackets(packets)
			if n != len(packets) || err != nil {
				t.Fatalf("write %d: %v", n, err)
			}
			if dev.calls != (len(packets)+bs-1)/bs {
				t.Fatalf("calls=%d", dev.calls)
			}
			for i, p := range original {
				if !bytes.Equal(p, packets[i]) || !bytes.Equal(p, dev.written[i]) {
					t.Fatalf("packet %d mutated or reordered", i)
				}
			}
		})
	}
}
func TestBatchDeviceWriteCounts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
		err   error
	}{
		{"short", 1, nil}, {"zero", 0, nil}, {"error", 300, io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev := &mutatingWriteDevice{fakeDevice: fakeDevice{batch: 4}, count: tc.count, err: tc.err}
			writer, ok := any(newBatchDevice(dev)).(interface{ WritePackets([][]byte) (int, error) })
			if !ok {
				t.Fatal("missing WritePackets adapter")
			}
			n, err := writer.WritePackets([][]byte{make([]byte, 20), make([]byte, 40)})
			if dev.calls != 1 {
				t.Fatal("retried uncertain write")
			}
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatal(err)
				}
				if runtime.GOOS == "linux" && n != 0 {
					t.Fatalf("Linux bytes treated as prefix: %d", n)
				}
			} else if runtime.GOOS == "linux" {
				if n != 2 || err != nil {
					t.Fatalf("Linux successful bytes result %d: %v", n, err)
				}
			} else if !errors.Is(err, io.ErrShortWrite) || n != tc.count {
				t.Fatalf("short count %d: %v", n, err)
			}
		})
	}
}

// Exercise Linux byte totals on every host, including totals smaller than the
// input count after GRO and errors with a large, non-prefix byte total.
func TestBatchDevicePlatformWriteResult(t *testing.T) {
	for _, osName := range []string{"linux", "darwin", "windows"} {
		for _, count := range []int{0, 1, 2, 4000} {
			for _, writeErr := range []error{nil, io.ErrUnexpectedEOF} {
				n, err := normalizeWriteResult(osName, 2, count, writeErr)
				if osName == "linux" {
					want := 2
					if writeErr != nil {
						want = 0
					}
					if n != want || err != writeErr {
						t.Fatalf("Linux bytes=%d: %d %v", count, n, err)
					}
				} else {
					if count > 2 {
						if n != 0 || err == nil {
							t.Fatal("invalid packet count accepted")
						}
					} else if n != count {
						t.Fatal("lost packet prefix")
					}
					if writeErr != nil && !errors.Is(err, writeErr) {
						t.Fatal("lost device error")
					}
					if writeErr == nil && count < 2 && !errors.Is(err, io.ErrShortWrite) {
						t.Fatal("missing short-write error")
					}
				}
			}
		}
	}
}

// stage4DirectReader is a private algorithm seam, not native qualification.
// Fakes exercise the same helper reached by qualified production Linux reads.
type stage4DirectReader interface {
	readPacketsDirect([][]byte, []int, int) (int, error)
}

type stage4ReadDevice struct {
	fakeDevice
	err      error
	before   func([][]byte, []int, int)
	reported int
}

func (f *stage4ReadDevice) Read(b [][]byte, s []int, off int) (int, error) {
	if f.before != nil {
		f.before(b, s, off)
	}
	n, e := f.fakeDevice.Read(b, s, off)
	if e != nil {
		return n, e
	}
	if f.reported >= 0 {
		n = f.reported
	}
	return n, f.err
}

func TestStage4DirectReadOwnership(t *testing.T) {
	for _, off := range []int{0, 3} {
		for _, size := range []int{0, 1, 20, 40, device.MaxSegmentSize - 2, device.MaxSegmentSize - 1, device.MaxSegmentSize} {
			t.Run(fmt.Sprintf("off=%d/size=%d", off, size), func(t *testing.T) {
				packets := [][]byte{bytes.Repeat([]byte{0x45}, size), bytes.Repeat([]byte{0x60}, 40)}
				f := &stage4ReadDevice{fakeDevice: fakeDevice{batch: 4, reads: [][][]byte{packets, {{0x45, 1, 2}}}}, err: io.ErrUnexpectedEOF, reported: -1}
				d := newBatchDevice(f)
				direct, ok := any(d).(stage4DirectReader)
				if !ok {
					t.Fatal("missing same-base direct algorithm")
				}
				dst := make([][]byte, 4)
				sizes := make([]int, 4)
				original := map[*byte]int{}
				for i := range dst {
					dst[i] = make([]byte, device.MaxSegmentSize+1, off+device.MaxSegmentSize+16)
					original[&dst[i][0]] = len(dst[i])
				}
				f.before = func(b [][]byte, s []int, o int) {
					if o != off || len(b) != len(dst) {
						t.Fatal("wrong native arguments")
					}
					for i := range b {
						if len(b[i]) != off+device.MaxSegmentSize || &b[i][0] != &dst[i][0] {
							t.Fatal("not expanded same-base alias")
						}
					}
				}
				n, e := direct.readPacketsDirect(dst, sizes, off)
				want := 1
				if size > 0 && size <= device.MaxSegmentSize+1-off {
					want++
				}
				if n != want || !errors.Is(e, io.ErrUnexpectedEOF) {
					t.Fatalf("read %d %v want %d", n, e, want)
				}
				valid := packets
				if want == 1 {
					valid = packets[1:]
				}
				for i, p := range valid {
					if sizes[i] != len(p) || !bytes.Equal(dst[i][off:off+sizes[i]], p) {
						t.Fatal("compaction lost packet")
					}
				}
				seen := map[*byte]bool{}
				for _, b := range dst {
					base := &b[0]
					if seen[base] || original[base] != len(b) {
						t.Fatal("lost/duplicate base or logical length")
					}
					seen[base] = true
				}
				if d.rbufs != nil || d.rsizes != nil {
					t.Fatal("direct path allocated scratch")
				}
				aliasBase := &d.readAliases[0]
				// Hold the first frame while replacing its slot for the next read.
				held := dst[0]
				snapshot := append([]byte(nil), held...)
				dst[0] = make([]byte, device.MaxSegmentSize+1, off+device.MaxSegmentSize+16)
				f.err = nil
				if n, e = direct.readPacketsDirect(dst, sizes, off); n != 1 || e != nil {
					t.Fatalf("next read %d %v", n, e)
				}
				if !bytes.Equal(held, snapshot) {
					t.Fatal("next read overwrote held frame")
				}
				if &d.readAliases[0] != aliasBase {
					t.Fatal("alias vector not reused")
				}
				for _, alias := range d.readAliases {
					if alias != nil {
						t.Fatal("adapter retains caller frame")
					}
				}
			})
		}
	}
}

func TestStage4ReadReportedPrefix(t *testing.T) {
	for _, directMode := range []bool{false, true} {
		for _, reported := range []int{0, 1, 2} {
			f := &stage4ReadDevice{fakeDevice: fakeDevice{batch: 4, reads: [][][]byte{{{1}, {2}, {3}}}}, err: tun.ErrTooManySegments, reported: reported}
			d := newBatchDevice(f)
			dst := make([][]byte, 4)
			sizes := make([]int, 4)
			for i := range dst {
				dst[i] = make([]byte, 65536, 65538)
			}
			read := d.ReadPackets
			if directMode {
				a, ok := any(d).(stage4DirectReader)
				if !ok {
					t.Fatal("missing direct reader")
				}
				read = a.readPacketsDirect
			}
			n, e := read(dst, sizes, 3)
			if n != reported || e != nil || f.readIdx != 1 {
				t.Fatalf("inferred remainder or retried: %d %v reads=%d", n, e, f.readIdx)
			}
		}
	}
}

func TestStage4ReadLogicalSiblingLimits(t *testing.T) {
	for _, directMode := range []bool{false, true} {
		for _, capacity := range []int{65536, 65538} {
			if directMode && capacity == 65536 {
				continue
			} // unqualified at offset 3
			f := &fakeDevice{batch: 4, reads: [][][]byte{{bytes.Repeat([]byte{1}, 100), bytes.Repeat([]byte{2}, 40), bytes.Repeat([]byte{3}, 60)}}}
			d := newBatchDevice(f)
			dst := [][]byte{make([]byte, 50, capacity), make([]byte, 100, capacity), make([]byte, 80, capacity), make([]byte, 65536, capacity)}
			sizes := make([]int, 4)
			original := append([][]byte(nil), dst...)
			read := d.ReadPackets
			if directMode {
				read = d.readPacketsDirect
			}
			n, e := read(dst, sizes, 3)
			if n != 2 || e != nil || sizes[0] != 40 || sizes[1] != 60 {
				t.Fatalf("sibling filtering %d %v %v", n, e, sizes)
			}
			if directMode {
				if &dst[0][0] != &original[1][0] || &dst[1][0] != &original[2][0] || &dst[2][0] != &original[0][0] || len(dst[0]) != 100 || len(dst[1]) != 80 || len(dst[2]) != 50 {
					t.Fatal("compaction copied or lost logical slices")
				}
			} else if len(d.rbufs) != 4 {
				t.Fatal("fallback omitted full scratch")
			}
		}
	}
}

func TestStage4ReadCompatibilityPendingError(t *testing.T) {
	f := &stage4ReadDevice{fakeDevice: fakeDevice{batch: 4, reads: [][][]byte{{{1}, {2}}, {{3}}}}, err: io.ErrUnexpectedEOF, reported: -1}
	d := newBatchDevice(f)
	b := make([]byte, 100)
	for _, want := range []byte{1, 2} {
		n, e := d.Read(b)
		if n != 1 || e != nil || b[0] != want {
			t.Fatalf("queued packet=%d n=%d err=%v", b[0], n, e)
		}
	}
	if n, e := d.Read(b); n != 0 || e != io.ErrUnexpectedEOF || f.readIdx != 1 {
		t.Fatalf("pending error=%d %v reads=%d", n, e, f.readIdx)
	}
	f.err = nil
	if n, e := d.Read(b); n != 1 || e != nil || b[0] != 3 {
		t.Fatalf("error repeated: %d %v", n, e)
	}
}

func TestStage4ReadInvalidArguments(t *testing.T) {
	for _, tc := range []struct {
		dst     [][]byte
		sizes   []int
		off     int
		wantErr bool
	}{
		{nil, nil, 0, false},
		{[][]byte{make([]byte, 10)}, nil, 0, true},
		{[][]byte{make([]byte, 10)}, make([]int, 1), -1, true},
		{[][]byte{make([]byte, 2)}, make([]int, 1), 3, true},
	} {
		f := &stage4ReadDevice{before: func([][]byte, []int, int) { t.Fatal("invalid/empty read consumed device IO") }}
		d := newBatchDevice(f)
		n, e := d.ReadPackets(tc.dst, tc.sizes, tc.off)
		if n != 0 || (e != nil) != tc.wantErr {
			t.Fatalf("invalid arguments result %d %v", n, e)
		}
	}
}

func TestStage4ReadQualification(t *testing.T) {
	d := newBatchDevice(&fakeDevice{batch: 128})
	q, ok := any(d).(interface {
		canReadDirect([][]byte, []int, int) bool
	})
	if !ok {
		t.Fatal("missing native qualification")
	}
	for _, off := range []int{0, 3} {
		for _, capacity := range []int{65536, 65538} {
			b := [][]byte{make([]byte, 65536, capacity)}
			if q.canReadDirect(b, make([]int, 1), off) {
				t.Fatal("fake qualified as NativeTun")
			}
		}
	}
	// Even ample capacity on a nonnative must use the full scratch fallback.
	f := &fakeDevice{batch: 128, reads: [][][]byte{{{0x45}}}}
	d = newBatchDevice(f)
	buf := config.LPool.New().([]byte)
	if n, e := d.ReadPackets([][]byte{buf}, make([]int, 1), 3); n != 1 || e != nil || len(d.rbufs) != 128 || len(d.rbufs[0]) != tunOffset+device.MaxSegmentSize {
		t.Fatal("nonnative fallback changed")
	}
}

// TestBatchDeviceReadNetConn verifies the net.Conn Read shim hands out one packet per call,
// refilling from a batched read (the surplus is buffered).
func TestBatchDeviceReadNetConn(t *testing.T) {
	p1 := []byte("aaaa")
	p2 := []byte("bbbbbb")
	bd := newBatchDevice(&fakeDevice{batch: 4, reads: [][][]byte{{p1, p2}}})

	b := make([]byte, 100)
	n, err := bd.Read(b)
	if err != nil {
		t.Fatalf("Read 1: %v", err)
	}
	if got := b[:n]; !bytes.Equal(got, p1) {
		t.Errorf("read 1 = %q, want %q", got, p1)
	}
	n, err = bd.Read(b)
	if err != nil {
		t.Fatalf("Read 2: %v", err)
	}
	if got := b[:n]; !bytes.Equal(got, p2) {
		t.Errorf("read 2 = %q, want %q", got, p2)
	}
}
