package core

import (
	"context"
	"net"
	"sync/atomic"

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

// Type prefix values carried in data[2] (1 byte, typePrefixLen). It is a small extensible
// discriminator: values 2..255 are reserved for future packet types (control frames,
// heartbeat tags, etc.).
const (
	// packetTypeToTUN marks a gvisor-processed packet (e.g. a response from the real
	// network) to be written straight to the TUN device.
	packetTypeToTUN byte = 0
	// packetTypeToGvisor marks a raw IP packet to be injected into the local gvisor stack.
	packetTypeToGvisor byte = 1
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
	// refs counts EXTRA references beyond the implicit owner: the zero value means a
	// single owner, so a freshly created Packet (including a plain &Packet{}) is valid
	// and is freed by exactly one release(). acquire() adds a reference; release()
	// removes one and returns the pooled buffer to config.LPool when the last (implicit)
	// reference is dropped. This lets one buffer be shared by N consumers (heartbeat
	// fan-out across conn slots, or an async bufferedTCP queue) without copying — each
	// consumer release()s when done and the last one frees. Modeled on gvisor's chunk
	// refcount (buffer.View.Clone / Release).
	refs atomic.Int32
}

// acquire adds a reference to the packet's buffer. Pair every acquire with a release.
func (p *Packet) acquire() { p.refs.Add(1) }

// release drops a reference; the pooled buffer is returned to config.LPool when the
// implicit owner's reference is dropped (refs goes to -1). Releasing again is a
// lifecycle bug that would otherwise surface as a hard-to-debug use-after-free, so
// fail loudly instead.
func (p *Packet) release() {
	switch n := p.refs.Add(-1); {
	case n == -1:
		config.LPool.Put(p.data[:])
	case n < -1:
		panic("core: Packet released more times than acquired")
	}
}

// NewPacket creates a Packet with the given buffer, length, and parsed IP addresses.
// The returned packet has a single (implicit) reference to data; see Packet.refs.
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
// The PacketBuffer's ref count is decremented before returning.
func copyPacketToPool(pkt *stack.PacketBuffer, prefix byte, headroom int) (buf []byte, length int) {
	buf = config.LPool.Get().([]byte)[:]
	// Copy straight from gvisor's section views into the pooled buffer. AsSlices aliases
	// the views (no copy), so this is a single copy out; ToView().AsSlice() would first
	// flatten the views into a throwaway buffer (a second copy).
	dst := buf[headroom+typePrefixLen:]
	n := 0
	for _, s := range pkt.AsSlices() {
		n += copy(dst[n:], s)
	}
	buf[headroom] = prefix
	pkt.DecRef()
	return buf, n + typePrefixLen
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
