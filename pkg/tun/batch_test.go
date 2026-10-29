package tun

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"testing"

	"golang.zx2c4.com/wireguard/tun"
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
		if cap(b) < offset+65535 {
			return 0, errors.New("insufficient GRO scratch")
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
