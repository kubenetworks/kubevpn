package tunnel

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/sniffer"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	framing "github.com/wencaiwulue/kubevpn/v2/pkg/internal/wire"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
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

type packetKind = byte

const (
	packetKindToTUN packetKind = iota
	packetKindToGvisor
	packetKindControl
)

// Type prefix values carried in data[2] (1 byte, typePrefixLen). It is a small extensible
// discriminator: values 3..255 are reserved for future packet types.
const (
	// packetTypeToTUN marks a gvisor-processed packet (e.g. a response from the real
	// network) to be written straight to the TUN device.
	packetTypeToTUN = packetKindToTUN
	// packetTypeToGvisor marks a raw IP packet to be injected into the local gvisor stack.
	packetTypeToGvisor = packetKindToGvisor
	// packetTypeControl marks a control-plane frame (heartbeat ICMP). A conn whose first
	// datagram carries this type is a control conn: the server does NOT register it in
	// Router and handles it in a dedicated control loop.
	packetTypeControl = packetKindControl
)

// packet represents a network packet with source and destination addresses.
// Do not copy a packet after construction. Every view is borrowed and becomes
// invalid at final release; a recycled object must never be accessed via an old pointer.
//
// Canonical buffer layout (single, system-wide):
//
//	data[0:2]            = datagram length header (datagramHeaderLen)
//	data[2]              = type prefix (typePrefixLen)
//	data[3:]             = raw IP payload (starts at tunReserve)
//	length               = typePrefixLen + len(IP)  // type + IP
//	wire frame           = data[0 : datagramHeaderLen+length]
//	raw IP               = data[tunReserve : datagramHeaderLen+length]
type packet struct {
	data   []byte
	view   *buffer.View
	length int
	src    net.IP
	dst    net.IP
	// refs counts EXTRA references beyond the implicit owner: the zero value means a
	// single owner, so a freshly created packet (including a plain &packet{}) is valid
	// and is freed by exactly one release(). acquire() adds a reference; release()
	// removes one and returns the pooled buffer to config.LPool when the last (implicit)
	// reference is dropped. This lets one buffer be shared by N consumers (heartbeat
	// fan-out across conn slots, or an async bufferedTCP queue) without copying — each
	// consumer release()s when done and the last one frees. Modeled on gvisor's chunk
	// refcount (buffer.View.Clone / Release).
	refs        atomic.Int32
	probeEpoch  uint64
	probeQueued bool
}

var packetPool = sync.Pool{New: func() any { return new(packet) }}

// acquire adds a reference to the packet's buffer. Pair every acquire with a release.
// The caller must already hold a live reference; an object must never be acquired
// again after its final release or after it has been recycled.
func (p *packet) acquire() {
	if p.refs.Load() < 0 {
		panic("core: packet acquired after final release")
	}
	p.refs.Add(1)
}

// release drops a reference; the data buffer and packet object are recycled when the
// implicit owner's reference is dropped (refs goes to -1). Releasing again is a
// lifecycle bug that would otherwise surface as a hard-to-debug use-after-free, so
// fail loudly instead.
func (p *packet) release() {
	switch n := p.refs.Add(-1); {
	case n == -1:
		data := p.data
		view := p.view
		p.data = nil
		p.view = nil
		p.length = 0
		p.src = nil
		p.dst = nil
		p.probeEpoch = 0
		p.probeQueued = false
		if data != nil {
			config.LPool.Put(data[:])
		}
		if view != nil {
			view.Release()
		}
		packetPool.Put(p)
	case n < -1:
		panic("core: packet released more times than acquired")
	}
}

// newPacket creates a packet with the given buffer, length, and parsed IP addresses.
// The returned packet has a single (implicit) reference to data; see packet.refs.
func newPacket(data []byte, length int, src net.IP, dst net.IP) *packet {
	p := packetPool.Get().(*packet)
	p.data = data
	p.view = nil
	p.length = length
	p.src = src
	p.dst = dst
	p.probeEpoch = 0
	p.probeQueued = false
	p.refs.Store(0)
	return p
}

// newPacketFromPool allocates a packet buffer with room for size bytes. Buffers
// returned to the pool retain the existing LPool ownership policy.
func newPacketFromPool(size int) *packet {
	if size > config.LargeBufferSize {
		return newPacket(make([]byte, size), 0, nil, nil)
	}
	return newPacket(packetBufferFromPool(), 0, nil, nil)
}

func packetBufferFromPool() []byte {
	return config.LPool.Get().([]byte)[:]
}

// newViewPacket wraps an exactly sized ingress View. The packet owns the View
// until it is released, materialized for wire output, or transferred to gVisor.
func newViewPacket(view *buffer.View) *packet {
	p := newPacket(nil, view.Size(), nil, nil)
	p.view = view
	return p
}

// materialize converts an ingress View into the canonical pooled layout used by
// asynchronous routing and TUN delivery. Addresses are reparsed from the new
// storage so they never alias a recycled View.
func (p *packet) materialize() error {
	if p.view == nil {
		return nil
	}
	data, src, dst, err := copyViewToPool(p.view)
	p.view.Release()
	p.view = nil
	if err != nil {
		return err
	}
	p.data = data
	p.src, p.dst = src, dst
	return nil
}

// copyViewToPool copies a framed payload into an LPool buffer and reparses
// addresses from that buffer. The returned src/dst never alias the input View.
func copyViewToPool(view *buffer.View) (buf []byte, src, dst net.IP, err error) {
	buf = packetBufferFromPool()
	frame := view.AsSlice()
	copy(buf[datagramHeaderLen:], frame)
	src, dst, _, err = netutil.ParseIPFast(buf[tunReserve:])
	if err != nil {
		config.LPool.Put(buf)
		return nil, nil, nil, err
	}
	return buf, src, dst, nil
}

// gvisorPayload transfers an ingress View without copying when possible. The
// caller still owns the packet metadata and must release it independently.
func (p *packet) gvisorPayload() buffer.Buffer {
	if p.view != nil {
		p.view.TrimFront(typePrefixLen)
		view := p.view
		p.view = nil
		return buffer.MakeWithView(view)
	}
	return buffer.MakeWithData(p.ip())
}

// Data returns the raw packet buffer (includes framing headers and payload).
func (d *packet) Data() []byte {
	_ = d.materialize()
	return d.data
}

// Length returns the number of meaningful bytes in the packet (excluding the leading headroom).
func (d *packet) Length() int {
	return d.length
}

// ip returns a borrowed view of the raw IP packet, valid until p is released
// or materialized into a different representation.
func (p *packet) ip() []byte {
	if p.view != nil {
		return p.view.AsSlice()[typePrefixLen:]
	}
	return p.data[tunReserve : datagramHeaderLen+p.length]
}

// setIP copies a raw IP packet into the packet and updates its payload length.
func (p *packet) setIP(ip []byte) {
	if p.materialize() != nil {
		return
	}
	p.length = typePrefixLen + copy(p.data[tunReserve:], ip)
}

// frame returns a borrowed view of the wire frame. The wire writer stamps its
// length immediately before I/O; an ingress View is materialized on first use.
func (p *packet) frame() []byte {
	if p.materialize() != nil {
		return nil
	}
	return p.data[:datagramHeaderLen+p.length]
}

// frameLen returns the number of bytes in the wire frame.
func (p *packet) frameLen() int {
	return datagramHeaderLen + p.length
}

func (p *packet) kind() packetKind {
	if p.view != nil {
		return p.view.AsSlice()[0]
	}
	return p.data[datagramHeaderLen]
}

func (p *packet) packetType() byte {
	return p.kind()
}

func (p *packet) setType(packetType packetKind) {
	if p.view != nil {
		p.view.AsSlice()[0] = packetType
		return
	}
	p.data[datagramHeaderLen] = packetType
}

// writeTo stamps and writes a tunnel frame without changing partial-write progress semantics.
func (p *packet) writeTo(w io.Writer) (int, error) {
	if err := p.materialize(); err != nil {
		return 0, err
	}
	return framing.WriteDatagramProgress(w, p.data, p.length)
}

// copyPacketToPool copies a gvisor packet into an owning packet with a 1-byte type prefix.
// headroom reserves extra bytes before the prefix for framing headers (e.g. 2-byte datagram length).
// The returned length is prefix + IP data, NOT including headroom. The PacketBuffer's
// ref count is decremented before returning; the caller owns the returned packet.
func copyPacketToPool(pkt *stack.PacketBuffer, prefix byte, headroom int) *packet {
	packet := newPacketFromPool(config.LargeBufferSize)
	buf := packet.data
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
	packet.length = n + typePrefixLen
	return packet
}

// logIPPacket logs one bare IP packet (no framing prefix) via gvisor's sniffer, which renders
// full transport-layer detail (ports, TCP flags/seq/ack/win, ICMP types, etc.). It is gated on
// the ctx logger's level (IsDebugEnabled) so the parse+format cost is skipped unless this
// connection is running at Debug — i.e. per-packet logging follows the connection's req.Level.
func logIPPacket(ctx context.Context, label string, data []byte) {
	if !plog.IsDebugEnabled(ctx) {
		return
	}
	var protocol tcpip.NetworkProtocolNumber
	if netutil.IsIPv4(data) {
		protocol = header.IPv4ProtocolNumber
	} else if netutil.IsIPv6(data) {
		protocol = header.IPv6ProtocolNumber
	} else {
		return
	}
	var dir sniffer.Direction = sniffer.DirectionSend
	if strings.Contains(label, "INBOUND") {
		dir = sniffer.DirectionRecv
	}
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(data),
	})
	sniffer.LogPacket(label+" ", dir, protocol, pkt)
	pkt.DecRef()
}

// sniffLink wraps a gvisor link endpoint with a per-packet sniffer only when the ctx logger is at
// Debug, otherwise returns the endpoint unwrapped. This keeps gvisor's built-in packet logging
// (which writes straight to the log file via glog, bypassing the per-RPC logger's level) tied to
// the connection's req.Level: a non-debug connection's stack is never wrapped, so it emits no
// per-packet lines and pays no sniffer overhead.
func sniffLink(ctx context.Context, ep stack.LinkEndpoint, prefix string) stack.LinkEndpoint {
	if !plog.IsDebugEnabled(ctx) {
		return ep
	}
	return sniffer.NewWithPrefix(ep, prefix)
}

// logStackPacket logs a gvisor stack packet via the sniffer, gated on the ctx logger's level so
// the pump goroutines only format+emit when this connection runs at Debug (follows req.Level).
func logStackPacket(ctx context.Context, prefix string, dir sniffer.Direction, protocol tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) {
	if !plog.IsDebugEnabled(ctx) {
		return
	}
	sniffer.LogPacket(prefix, dir, protocol, pkt)
}
