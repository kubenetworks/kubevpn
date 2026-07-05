package tun

import (
	"bytes"
	"io"
	"os"
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

func (f *fakeDevice) File() *os.File          { return nil }
func (f *fakeDevice) MTU() (int, error)       { return 1500, nil }
func (f *fakeDevice) Name() (string, error)   { return "faketun", nil }
func (f *fakeDevice) Events() <-chan tun.Event { return nil }
func (f *fakeDevice) Close() error            { return nil }

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
