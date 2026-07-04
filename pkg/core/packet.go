package core

import (
	"context"
	"net"

	"github.com/google/gopacket/layers"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

const (
	// datagramHeaderLen is the 2-byte big-endian length prefix that frames each
	// datagram on the wire: [2-byte length][payload].
	datagramHeaderLen = 2
	// typePrefixLen is the 1-byte type prefix in front of the IP payload
	// (0 = write back to TUN, 1 = inject into the local gvisor stack).
	typePrefixLen = 1
	// tunReserve is the headroom reserved at the front of every pooled buffer when
	// reading from the TUN: [datagramHeaderLen][typePrefixLen]. It lets the datagram
	// length and type prefix be written in place, without copying, on the way out.
	tunReserve = datagramHeaderLen + typePrefixLen // 3
)

// Packet represents a network packet with source and destination addresses.
//
// Canonical buffer layout (single, system-wide):
//
//	data[0:2]            = datagram length header (datagramHeaderLen)
//	data[2]              = type prefix (typePrefixLen)
//	data[3:]             = raw IP payload (starts at tunReserve)
//	length               = typePrefixLen + len(IP)  // type + IP
//	wire frame           = data[0 : datagramHeaderLen+length]
//	raw IP               = data[tunReserve : datagramHeaderLen+length]
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

// copyPacketToPool copies a gvisor packet into a pool buffer with a 1-byte type prefix.
// headroom reserves extra bytes before the prefix for framing headers (e.g. 2-byte datagram length).
// Returns the buffer and payload length (prefix + IP data, NOT including headroom).
// Caller must return buf to config.LPool.
// The PacketBuffer's view is released and its ref count decremented before returning.
func copyPacketToPool(pkt *stack.PacketBuffer, prefix byte, headroom int) (buf []byte, length int) {
	view := pkt.ToView()
	data := view.AsSlice()
	buf = config.LPool.Get().([]byte)[:]
	n := copy(buf[headroom+typePrefixLen:], data)
	buf[headroom] = prefix
	view.Release()
	pkt.DecRef()
	return buf, n + 1
}

// logIPPacket logs one bare IP packet (no framing prefix) at Debug with a direction label
// (e.g. "[Client] OUTBOUND", "[Client-2] INBOUND", "[TUN]"), when config.Debug is on. It is
// the single place these data-plane files render a packet for logging, confining the
// gopacket/layers dependency here.
func logIPPacket(ctx context.Context, label string, data []byte) {
	if !config.Debug {
		return
	}
	if src, dst, proto, err := util.ParseIPFast(data); err == nil {
		plog.G(ctx).Debugf("%s SRC: %s, DST: %s, Protocol: %s, Length: %d",
			label, src, dst, layers.IPProtocol(proto).String(), len(data))
	}
}
