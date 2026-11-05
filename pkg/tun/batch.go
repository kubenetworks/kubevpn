package tun

import (
	"context"
	"errors"
	"io"
	"runtime"
	"sync"

	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// tunOffset is the per-buffer headroom wireguard-go requires for every packet handed to
// tun.Device.Read/Write. Read places the IP packet at buf[tunOffset:]; the Linux write
// path (GSO) needs at least virtioNetHdrLen(10) bytes of headroom before the packet to
// encode the virtio-net header in place. MessageTransportOffsetContent(16) satisfies both
// and matches wireguard-go's own internal convention.
const tunOffset = device.MessageTransportOffsetContent

// batchDevice adapts wireguard-go's batched TUN API to the data plane. Linux
// NativeTun.Read splits GSO into caller-provided buffers without replacing their
// bases; adequately sized buffers can therefore receive directly at the framing
// offset. Other devices use full-size scratch and copy. Linux Write performs GRO
// mutation/coalescing, so writes always copy into private scratch with headroom.
type batchDevice struct {
	dev tun.Device

	eventsDone chan struct{}
	closeOnce  sync.Once
	closeErr   error

	rmu         sync.Mutex
	rbufs       [][]byte
	rsizes      []int
	readAliases [][]byte

	wmu   sync.Mutex
	wbufs [][]byte

	// net.Conn Read compatibility: surplus packets from a batched read, one per Read call.
	lmu            sync.Mutex
	leftover       [][]byte
	pendingReadErr error
}

func newBatchDevice(dev tun.Device) *batchDevice {
	d := &batchDevice{dev: dev, eventsDone: make(chan struct{})}
	events := dev.Events()
	if events == nil {
		close(d.eventsDone)
		return d
	}
	go func() {
		defer close(d.eventsDone)
		for range events {
		}
	}()
	return d
}

// BatchSize is the max number of packets a single Read/Write may handle.
func (d *batchDevice) BatchSize() int { return d.dev.BatchSize() }

func (d *batchDevice) scratchLen() int { return tunOffset + device.MaxSegmentSize }

func (d *batchDevice) initRead() {
	if d.rbufs != nil {
		return
	}
	n := d.dev.BatchSize()
	d.rbufs = make([][]byte, n)
	d.rsizes = make([]int, n)
	for i := range d.rbufs {
		d.rbufs[i] = make([]byte, d.scratchLen())
	}
}

// canReadDirect deliberately qualifies the concrete native type, not BatchSize
// or MTU: neither bounds a Linux read segment. Check every destination before IO.
func (d *batchDevice) canReadDirect(dst [][]byte, sizes []int, off int) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if _, ok := d.dev.(*tun.NativeTun); !ok {
		return false
	}
	if off < 0 || len(dst) == 0 || len(sizes) < len(dst) {
		return false
	}
	for _, buf := range dst {
		if off > len(buf) || cap(buf)-off < device.MaxSegmentSize {
			return false
		}
	}
	return true
}

// readPacketsDirect requires qualified destinations and the caller's read lock.
// Kept separate from native qualification so fake devices can test the algorithm.
func (d *batchDevice) readPacketsDirect(dst [][]byte, sizes []int, off int) (int, error) {
	if cap(d.readAliases) < len(dst) {
		d.readAliases = make([][]byte, len(dst))
	}
	d.readAliases = d.readAliases[:len(dst)]
	defer clear(d.readAliases)
	for i, buf := range dst {
		d.readAliases[i] = buf[:off+device.MaxSegmentSize]
	}
	n, err := d.dev.Read(d.readAliases, sizes, off)
	m := 0
	for i := 0; i < min(n, len(dst)); i++ {
		size := sizes[i]
		if size <= 0 {
			continue
		}
		if size > len(dst[i])-off {
			plog.G(context.Background()).Warnf("[TUN] dropping oversized packet (%d bytes) that cannot be framed", size)
			continue
		}
		// Swap whole logical slices: retain every original base exactly once,
		// including buffers of dropped siblings. Never copy packet payloads.
		dst[m], dst[i] = dst[i], dst[m]
		sizes[m] = size
		m++
	}
	if errors.Is(err, tun.ErrTooManySegments) {
		plog.G(context.Background()).Warnf("[TUN] too many segments in one read, surplus dropped: %v", err)
		err = nil
	}
	return m, err
}

// ReadPackets returns a compact valid prefix at dst[i][off:off+sizes[i]].
// Oversized segments are dropped against logical length, never backing capacity.
// ErrTooManySegments is nonfatal; only the reported prefix is consumed.
func (d *batchDevice) ReadPackets(dst [][]byte, sizes []int, off int) (int, error) {
	d.rmu.Lock()
	defer d.rmu.Unlock()
	if off < 0 || len(sizes) < len(dst) {
		return 0, errors.New("invalid tun read buffers")
	}
	if len(dst) == 0 {
		return 0, nil
	}
	for _, buf := range dst {
		if off > len(buf) {
			return 0, errors.New("invalid tun read offset")
		}
	}
	if d.canReadDirect(dst, sizes, off) {
		return d.readPacketsDirect(dst, sizes, off)
	}
	d.initRead()

	// Reset scratch slices before calling an unqualified device.
	for i := range d.rbufs {
		if cap(d.rbufs[i]) < d.scratchLen() {
			d.rbufs[i] = make([]byte, d.scratchLen())
		} else {
			d.rbufs[i] = d.rbufs[i][:d.scratchLen()]
		}
	}

	n, err := d.dev.Read(d.rbufs, d.rsizes, tunOffset)
	// n packets are valid even when err == ErrTooManySegments (partial batch).
	m := 0
	for i := 0; i < min(n, len(d.rbufs)) && m < len(dst); i++ {
		size := d.rsizes[i]
		if size <= 0 {
			continue
		}
		if size > len(dst[m])-off || size > len(d.rbufs[i])-tunOffset {
			plog.G(context.Background()).Warnf("[TUN] dropping oversized packet (%d bytes) that cannot be framed", size)
			continue
		}
		copy(dst[m][off:off+size], d.rbufs[i][tunOffset:tunOffset+size])
		sizes[m] = size
		m++
	}
	if err != nil && errors.Is(err, tun.ErrTooManySegments) {
		plog.G(context.Background()).Warnf("[TUN] too many segments in one read, surplus dropped: %v", err)
		err = nil
	}
	return m, err
}

// Read implements net.Conn by handing out one packet per call, refilling from a batched
// read when the surplus queue drains. This path is only used for the non-batch fallback
// (the TUN hot path uses ReadPackets); it copies for simplicity, not speed.
func (d *batchDevice) Read(b []byte) (int, error) {
	d.lmu.Lock()
	defer d.lmu.Unlock()
	for len(d.leftover) == 0 {
		if d.pendingReadErr != nil {
			err := d.pendingReadErr
			d.pendingReadErr = nil
			return 0, err
		}
		bs := d.dev.BatchSize()
		dst := make([][]byte, bs)
		sizes := make([]int, bs)
		for i := range dst {
			dst[i] = make([]byte, len(b))
		}
		n, err := d.ReadPackets(dst, sizes, 0)
		d.pendingReadErr = err
		for i := 0; i < n; i++ {
			pkt := make([]byte, sizes[i])
			copy(pkt, dst[i][:sizes[i]])
			d.leftover = append(d.leftover, pkt)
		}
	}
	p := d.leftover[0]
	d.leftover[0] = nil
	d.leftover = d.leftover[1:]
	return copy(b, p), nil
}

// Write implements net.Conn by writing a single IP packet through the batched device API,
// copying into scratch with the header room wireguard-go requires so b is not modified.
func (d *batchDevice) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	n, err := d.WritePackets([][]byte{b})
	if n == 1 {
		return len(b), err
	}
	return 0, err
}

// WritePackets copies IP packets into per-device scratch, preserving the caller's
// buffers even when Linux GRO mutates/coalesces them. It never retries a failed
// device write: on Linux an error provides bytes, not a trustworthy packet prefix.
func (d *batchDevice) WritePackets(packets [][]byte) (int, error) {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	limit := max(1, d.dev.BatchSize())
	complete := 0
	for len(packets) > 0 {
		n := min(limit, len(packets))
		if len(d.wbufs) < n {
			d.wbufs = append(d.wbufs, make([][]byte, n-len(d.wbufs))...)
		}
		for i, p := range packets[:n] {
			if len(p) > device.MaxSegmentSize {
				return complete, errors.New("tun packet too large to write")
			}
			if cap(d.wbufs[i]) < d.scratchLen() {
				d.wbufs[i] = make([]byte, d.scratchLen())
			}
			d.wbufs[i] = d.wbufs[i][:tunOffset+len(p)]
			clear(d.wbufs[i][:tunOffset])
			copy(d.wbufs[i][tunOffset:], p)
		}
		written, err := d.dev.Write(d.wbufs[:n], tunOffset)
		written, err = normalizeWriteResult(runtime.GOOS, n, written, err)
		complete += written
		if err != nil {
			return complete, err
		}
		packets = packets[n:]
	}
	return complete, nil
}

func normalizeWriteResult(goos string, packets, written int, err error) (int, error) {
	if goos == "linux" {
		if err != nil {
			return 0, err // byte total cannot identify which packets succeeded after GRO
		}
		return packets, nil
	}
	if written < 0 || written > packets {
		return 0, errors.Join(err, io.ErrShortWrite)
	}
	if err == nil && written != packets {
		err = io.ErrShortWrite
	}
	return written, err
}

// Close keeps draining while the native device stops its event producers, then
// joins the drainer. tun.Device.Close guarantees that the event channel closes.
func (d *batchDevice) Close() error {
	d.closeOnce.Do(func() {
		d.closeErr = d.dev.Close()
		<-d.eventsDone
	})
	return d.closeErr
}
