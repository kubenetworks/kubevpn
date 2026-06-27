package core

import (
	"context"
	"net"

	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

const (
	// MaxSize is the channel buffer capacity for packet queues (inbound, outbound, slots).
	MaxSize = 1000
)

// tunDevice holds the fields and methods shared between server Device and client ClientDevice.
type tunDevice struct {
	tun         net.Conn
	tunInbound  chan *Packet
	tunOutbound chan *Packet
	errChan     chan error
}

func (d *tunDevice) writeToTun(ctx context.Context) {
	defer util.HandleCrash()
	defer drainPacketChan(d.tunOutbound)
	for {
		select {
		case packet := <-d.tunOutbound:
			if packet == nil {
				return
			}
			_, err := d.tun.Write(packet.data[1:packet.length])
			config.LPool.Put(packet.data[:])
			if err != nil {
				plog.G(ctx).Errorf("[TUN] Failed to write to tun device: %v", err)
				util.SafeWrite(d.errChan, err)
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (d *tunDevice) Close() {
	d.tun.Close()
}

// copyPacketToPool copies a gvisor packet into a pool buffer with a 1-byte type prefix.
// headroom reserves extra bytes before the prefix for framing headers (e.g. 2-byte datagram length).
// Returns the buffer and payload length (prefix + IP data, NOT including headroom).
// Caller must return buf to config.LPool.
// The PacketBuffer's view is released and its ref count decremented before returning.
func copyPacketToPool(pkt *stack.PacketBuffer, prefix byte, headroom int) (buf []byte, length int) {
	view := pkt.ToView()
	data := view.AsSlice()
	buf = config.LPool.Get().([]byte)[:]
	n := copy(buf[headroom+1:], data)
	buf[headroom] = prefix
	view.Release()
	pkt.DecRef()
	return buf, n + 1
}

// Packet represents a network packet with source and destination addresses.
type Packet struct {
	data   []byte
	length int
	src    net.IP
	dst    net.IP
}

// NewPacket creates a Packet with the given buffer, length, and parsed IP addresses.
func NewPacket(data []byte, length int, src net.IP, dst net.IP) *Packet {
	return &Packet{
		data:   data,
		length: length,
		src:    src,
		dst:    dst,
	}
}

// Data returns the raw packet buffer (includes framing headers and payload).
func (d *Packet) Data() []byte {
	return d.data
}

// Length returns the number of meaningful bytes in the packet (excluding the leading headroom).
func (d *Packet) Length() int {
	return d.length
}
