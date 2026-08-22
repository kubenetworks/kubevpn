package tun

import (
	"context"
	"errors"
	"sync"

	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// tunOffset is the per-buffer headroom wireguard-go requires for every packet handed to
// tun.Device.Read/Write. Read places the IP packet at buf[tunOffset:]; the Linux write
// path (GSO) needs at least virtioNetHdrLen(10) bytes of headroom before the packet to
// encode the virtio-net header in place. MessageTransportOffsetContent(16) satisfies both
// and matches wireguard-go's own internal convention.
const tunOffset = device.MessageTransportOffsetContent

// batchDevice adapts a wireguard-go tun.Device (batched, offload/GRO-capable API) to the
// data plane. Since wireguard-go v0.0.20230223 the device reads/writes *multiple* packets
// per syscall via `Read(bufs, sizes, offset)` / `Write(bufs, offset)`, and with offload on
// a single Read may return several GRO-coalesced IP packets. This adapter:
//
//   - bridges the offset mismatch (wireguard wants the packet at buf[tunOffset:], the data
//     plane wants it at buf[tunReserve:]) with a single copy, exactly as the pre-batch code
//     already did — GRO's append-realloc of bufs[i] rules out sharing the buffer anyway;
//   - exposes ReadPackets for the segment-aware read hot path (one dispatch per packet);
//   - guards the tunnel frame boundary: a coalesced packet too large to fit the caller's
//     buffer (and thus the 2-byte tunnel length header) is dropped, TCP recovers via
//     retransmission;
//   - still satisfies net.Conn (Read/Write) for compatibility and the non-batch fallback.
type batchDevice struct {
	dev tun.Device

	rmu    sync.Mutex
	rbufs  [][]byte
	rsizes []int

	// net.Conn Read compatibility: surplus packets from a batched read, one per Read call.
	lmu      sync.Mutex
	leftover [][]byte
}

func newBatchDevice(dev tun.Device) *batchDevice { return &batchDevice{dev: dev} }

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

// ReadPackets reads up to len(dst) IP packets from the TUN in a single syscall. Packet i is
// copied into dst[i][off:off+sizes[i]] and its length written to sizes[i]; the number of
// packets placed into dst is returned. Packets too large to fit dst[i] (a GRO-coalesced
// packet beyond the tunnel frame limit) are dropped with a warning. ErrTooManySegments is
// treated as non-fatal: the packets that were read are returned and the error is swallowed.
func (d *batchDevice) ReadPackets(dst [][]byte, sizes []int, off int) (int, error) {
	d.rmu.Lock()
	defer d.rmu.Unlock()
	d.initRead()

	// Reset scratch slices: a prior read may have grown one via GRO append-realloc.
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
	for i := 0; i < n && m < len(dst); i++ {
		size := d.rsizes[i]
		if size <= 0 {
			continue
		}
		if off+size > len(dst[m]) {
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
		bs := d.dev.BatchSize()
		dst := make([][]byte, bs)
		sizes := make([]int, bs)
		for i := range dst {
			dst[i] = make([]byte, len(b))
		}
		n, err := d.ReadPackets(dst, sizes, 0)
		if err != nil {
			return 0, err
		}
		for i := 0; i < n; i++ {
			pkt := make([]byte, sizes[i])
			copy(pkt, dst[i][:sizes[i]])
			d.leftover = append(d.leftover, pkt)
		}
	}
	p := d.leftover[0]
	d.leftover = d.leftover[1:]
	return copy(b, p), nil
}

// Write implements net.Conn by writing a single IP packet through the batched device API,
// copying into scratch with the header room wireguard-go requires so b is not modified.
func (d *batchDevice) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	buf := config.LPool.Get().([]byte)[:]
	defer config.LPool.Put(buf[:])
	if tunOffset+len(b) > len(buf) {
		return 0, errors.New("tun packet too large to write")
	}
	copy(buf[tunOffset:tunOffset+len(b)], b)
	if _, err := d.dev.Write([][]byte{buf[:tunOffset+len(b)]}, tunOffset); err != nil {
		return 0, err
	}
	return len(b), nil
}

// Close closes the underlying device.
func (d *batchDevice) Close() error { return d.dev.Close() }
