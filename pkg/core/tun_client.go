package core

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

const (
	// ConnPoolSize is the number of parallel TCP connections to the server.
	// Each connection handles a subset of traffic (partitioned by five-tuple hash —
	// proto, dst IP, and both ports — falling back to dst IP hash for fragments/ICMP).
	// Multiple connections reduce head-of-line blocking and improve throughput.
	ConnPoolSize = 4
)

// clientTransport is the spoke/endpoint half of the tun device. Outbound packets read from the
// TUN are distributed across a pool of dialed connections to the server (by five-tuple hash), or
// looped back through a local gvisor stack when src == dst. Inbound packets from the server are
// delivered to the device's tunOutbound by the per-connection readers.
type clientTransport struct {
	dev     *tunDevice
	forward *Forwarder
	// slots holds the per-connection pool; built by runConnPool, read by routeOutbound.
	slots []*connSlot
	// gvisorInbound carries src == dst packets to the local loopback gvisor stack.
	gvisorInbound chan *Packet
	// stats records data-plane liveness from observed heartbeat echo replies; may be nil.
	stats *HeartbeatStats
	// poolSize overrides the number of parallel connections; <=0 means use ConnPoolSize.
	// Production leaves it 0 (ConnPoolSize); only benchmarks/tests set it to compare sizes.
	poolSize int
}

func newClientTransport(dev *tunDevice, forward *Forwarder, stats *HeartbeatStats) *clientTransport {
	return &clientTransport{
		dev:           dev,
		forward:       forward,
		gvisorInbound: make(chan *Packet, MaxSize),
		stats:         stats,
	}
}

func (t *clientTransport) label() string { return "[Client]" }

func (t *clientTransport) routines() []namedRoutine {
	return []namedRoutine{
		{"client-gvisor", func(ctx context.Context) {
			handleGvisorPacket(t.gvisorInbound, t.dev.tunOutbound, datagramHeaderLen).Run(ctx)
		}},
		{"client-conn-pool", t.runConnPool},
		{"client-heartbeat", t.heartbeats},
	}
}

// routeOutbound dispatches a packet read from the TUN: loop it back through the local gvisor
// stack when src == dst, otherwise enqueue it for the connection pool (distributed by five-tuple).
func (t *clientTransport) routeOutbound(ctx context.Context, buf []byte, n int, src, dst net.IP) {
	// buf is canonical (pumpTun reserved buf[0:tunReserve]): set the type prefix and the
	// IP already sits at buf[tunReserve:]. Both branches forward the same canonical buffer.
	buf[datagramHeaderLen] = packetTypeToGvisor
	logIPPacket(ctx, "[Client] OUTBOUND", buf[tunReserve:tunReserve+n])
	if src.Equal(dst) {
		t.gvisorInbound <- NewPacket(buf[:], n+typePrefixLen, src, dst)
	} else {
		// Enqueue to the shared tunInbound; runConnPool distributes to a slot by five-tuple hash.
		t.dev.tunInbound <- NewPacket(buf[:], n+typePrefixLen, src, dst)
	}
}

// runConnPool creates N parallel connection slots and distributes packets by five-tuple hash. Each
// slot runs independently — if one connection breaks, only that slot reconnects. It waits for all
// slot goroutines to exit before returning, for deterministic shutdown.
func (t *clientTransport) runConnPool(ctx context.Context) {
	n := t.poolSize
	if n <= 0 {
		n = ConnPoolSize
	}
	t.slots = make([]*connSlot, n)
	var wg sync.WaitGroup
	for i := range t.slots {
		slot := &connSlot{
			id:            i,
			inbound:       make(chan *Packet, MaxSize),
			tunOutbound:   t.dev.tunOutbound,
			forward:       t.forward,
			stats:         t.stats,
			registrations: t.registrationPayloads,
		}
		t.slots[i] = slot
		wg.Add(1)
		go func(s *connSlot) {
			defer wg.Done()
			defer netutil.HandleCrash()
			s.run(ctx)
		}(slot)
	}
	defer wg.Wait()

	// Drain the shared tunInbound and distribute to slots by five-tuple hash.
	// Heartbeats (dst == nil) are broadcast to ALL slots to keep idle connections alive.
	for {
		select {
		case packet := <-t.dev.tunInbound:
			if packet == nil {
				return
			}
			if packet.dst != nil {
				// IP payload is data[tunReserve : datagramHeaderLen+length] (see packet.go layout).
				key := parseFiveTupleInline(packet.data[tunReserve : datagramHeaderLen+packet.length])
				trySendToSlot(t.slots[flowHash(key, packet.dst, n)].inbound, packet)
			} else {
				broadcastToSlots(t.slots, packet)
			}
		case <-ctx.Done():
			return
		}
	}
}

// trySendToSlot delivers packet to a slot's inbound channel, dropping (and releasing) it if the
// channel is full so a slow slot cannot stall the others.
func trySendToSlot(inbound chan *Packet, packet *Packet) {
	select {
	case inbound <- packet:
	default:
		packet.release()
	}
}

// broadcastToSlots delivers the packet to every slot so an idle connection still receives
// heartbeats. The buffer is cloned per slot rather than shared: each slot's writeToConn stamps
// the datagram length header in place and the socket write reads the buffer, all concurrently,
// so a shared buffer would be a data race. Heartbeats are infrequent and small, so the clone is
// negligible. Each clone (and the original handed to slot 0) is freed by its slot via release().
func broadcastToSlots(slots []*connSlot, packet *Packet) {
	for i := 1; i < len(slots); i++ {
		clone := config.LPool.Get().([]byte)
		copy(clone, packet.data[:packet.length+datagramHeaderLen])
		trySendToSlot(slots[i].inbound, NewPacket(clone, packet.length, nil, nil))
	}
	if len(slots) == 0 {
		packet.release()
		return
	}
	trySendToSlot(slots[0].inbound, packet)
}

// ipHash returns a consistent slot index for an IP address.
// Uses FNV-1a-like hash for fast, well-distributed mapping.
func ipHash(ip net.IP, slots int) int {
	var h uint32 = 2166136261
	for _, b := range ip {
		h ^= uint32(b)
		h *= 16777619
	}
	return int(h % uint32(slots))
}

// flowKey holds the L4 fields used for five-tuple slot selection. The destination IP is
// not stored here — the caller already has it as packet.dst and passes it to flowHash —
// so this struct only carries what parseFiveTupleInline must dig out of the L4 header.
//
// hasPorts is false for packets that carry no usable L4 ports: IP fragments (only the
// first fragment has the L4 header), ICMP, and protocols/headers we do not parse. In
// that case flowHash falls back to dst-IP hashing, which keeps every fragment of one
// datagram (they share the dst IP) on the same slot and preserves legacy behavior.
type flowKey struct {
	proto    uint8
	srcPort  uint16
	dstPort  uint16
	hasPorts bool
}

// parseFiveTupleInline extracts the L4 protocol and ports from a raw IP packet (the bytes
// at packet.data[tunReserve:], i.e. starting at the IP version nibble). It is zero-alloc:
// it only reads a handful of bytes and returns a value type. The IP packet is assumed to
// have already passed ParseIPFast (so the version/length are sane), but every offset is
// still bounds-checked defensively — a malformed packet just yields hasPorts=false and is
// routed by dst IP.
//
// Only TCP/UDP/SCTP carry the 16-bit src/dst ports at offsets 0/2 of the L4 header.
// IPv4 fragments (MF set or fragment-offset != 0) and any other protocol fall back.
func parseFiveTupleInline(ipData []byte) flowKey {
	if len(ipData) < 1 {
		return flowKey{}
	}
	switch ipData[0] >> 4 {
	case 4:
		if len(ipData) < 20 {
			return flowKey{}
		}
		// IPv4 fragment: only the first fragment (offset 0, MF possibly set) carries the
		// L4 header; later fragments do not. Route the whole datagram by dst IP so every
		// fragment lands on the same slot. flagsFrag = [3-bit flags][13-bit frag offset];
		// bit 13 (0x2000) is MF, the low 13 bits are the offset.
		flagsFrag := binary.BigEndian.Uint16(ipData[6:8])
		if flagsFrag&0x2000 != 0 || flagsFrag&0x1fff != 0 {
			return flowKey{}
		}
		proto := ipData[9]
		ihl := int(ipData[0]&0x0f) * 4
		if ihl < 20 {
			return flowKey{}
		}
		return l4Ports(proto, ipData, ihl)
	case 6:
		if len(ipData) < 40 {
			return flowKey{}
		}
		// Extension headers are not walked: if the next header is not a port-bearing L4
		// protocol we fall back to dst-IP hashing. Extension-header packets are rare in
		// practice (the common case is bare TCP/UDP), so this is an accepted limitation.
		return l4Ports(ipData[6], ipData, 40)
	default:
		return flowKey{}
	}
}

// l4Ports reads the src/dst ports for port-bearing protocols, given the L4 header offset.
// Returns hasPorts=false (dst-IP fallback) for non-port protocols or a truncated header.
func l4Ports(proto uint8, ipData []byte, l4Off int) flowKey {
	switch proto {
	case 6, 17, 132: // TCP, UDP, SCTP
		if len(ipData) < l4Off+4 {
			return flowKey{}
		}
		return flowKey{
			proto:    proto,
			srcPort:  binary.BigEndian.Uint16(ipData[l4Off : l4Off+2]),
			dstPort:  binary.BigEndian.Uint16(ipData[l4Off+2 : l4Off+4]),
			hasPorts: true,
		}
	default:
		return flowKey{}
	}
}

// flowHash returns a consistent slot index for an L4 flow, mixing proto, dst IP, and both
// ports (FNV-1a, same style as ipHash). Spreading by the full five-tuple keeps many flows
// to one hot dst IP balanced across the pool, instead of pinning them all to one slot.
//
// The source IP is deliberately omitted: a client has a single TUN IP, so it adds nothing
// to the distribution (the analog of IPVS source-hashing using only the discriminating
// fields). When the packet has no usable ports (fragment/ICMP/unparsed), it falls back to
// ipHash(dstIP) so a flow's fragments stay together and legacy behavior is preserved.
//
// Determinism is a hard requirement: every packet of one flow MUST map to the same slot,
// because the server runs an independent gvisor stack per pool connection — splitting a
// flow across slots would land its packets on different stacks and corrupt TCP state.
func flowHash(key flowKey, dstIP net.IP, slots int) int {
	if !key.hasPorts {
		return ipHash(dstIP, slots)
	}
	var h uint32 = 2166136261
	h ^= uint32(key.proto)
	h *= 16777619
	for _, b := range dstIP {
		h ^= uint32(b)
		h *= 16777619
	}
	h ^= uint32(key.srcPort >> 8)
	h *= 16777619
	h ^= uint32(key.srcPort & 0xff)
	h *= 16777619
	h ^= uint32(key.dstPort >> 8)
	h *= 16777619
	h ^= uint32(key.dstPort & 0xff)
	h *= 16777619
	return int(h % uint32(slots))
}

// connSlot is one connection in the client pool. It owns its inbound packet channel and the
// dial/reconnect lifecycle, and runs the framed read/write loops over the connection. It
// replaces the previous slotID-threaded free functions.
type connSlot struct {
	id          int
	inbound     chan *Packet
	tunOutbound chan *Packet
	forward     *Forwarder
	// stats records data-plane liveness from observed heartbeat echo replies; may be nil.
	stats *HeartbeatStats
	// registrations returns the route-registration payloads to announce on this conn after each
	// (re)connect, so the server registers the route immediately instead of waiting for data or a
	// periodic heartbeat. Computed lazily (picks up the current TUN IP); may be nil in tests.
	registrations func() [][]byte
}

// run manages this slot's single connection with reconnect logic.
func (s *connSlot) run(ctx context.Context) {
	for ctx.Err() == nil {
		func() {
			subCtx, cancel := context.WithCancel(ctx)
			defer cancel()

			conn, err := s.forward.DialContext(subCtx)
			if err != nil {
				plog.G(ctx).Errorf("[Client-%d] Failed to dial %s: %v", s.id, s.forward.Addr, err)
				time.Sleep(config.SlotReconnectBackoff)
				return
			}
			defer conn.Close()
			plog.G(ctx).Debugf("[Client-%d] Connected to %s", s.id, conn.RemoteAddr())

			udpConn := conn.(*UDPConnOverTCP)

			// Proactively register this conn's route on the server: announce our TUN IP(s) right
			// away so the server can route reverse traffic to us immediately, without waiting for
			// the first data packet or a periodic heartbeat broadcast (which may be dropped under
			// load). Sent synchronously before the read/write loops start, so there is no competing
			// writer. Best-effort — a failure just falls through; the loops will catch a dead conn.
			if s.registrations != nil {
				for _, payload := range s.registrations() {
					if _, err = udpConn.Write(payload); err != nil {
						plog.G(ctx).Debugf("[Client-%d] Failed to send route registration: %v", s.id, err)
						break
					}
				}
			}

			errChan := make(chan error, 2)
			go s.readFromConn(subCtx, udpConn, errChan)
			go s.writeToConn(subCtx, udpConn.Conn, errChan)

			select {
			case err = <-errChan:
				plog.G(ctx).Warnf("[Client-%d] Disconnected from %s: %v", s.id, conn.RemoteAddr(), err)
			case <-ctx.Done():
				return
			}
		}()
	}
}

// readFromConn reads datagram-framed packets from the server, routing IP packets to the tun
// and gvisor-prefixed packets to this slot's local gvisor stack. conn is the framed
// *UDPConnOverTCP in production (typed as net.Conn so tests can drive it with a raw pipe).
func (s *connSlot) readFromConn(ctx context.Context, conn net.Conn, errChan chan error) {
	defer netutil.HandleCrash()
	gvisorInbound := make(chan *Packet, MaxSize)
	go handleGvisorPacket(gvisorInbound, s.inbound, datagramHeaderLen).Run(ctx)
	dl := &periodicDeadline{timeout: config.KeepAliveTime * 3, set: conn.SetReadDeadline}
	for ctx.Err() == nil {
		buf := config.LPool.Get().([]byte)[:]
		if err := dl.refresh(); err != nil {
			config.LPool.Put(buf[:])
			plog.G(ctx).Errorf("[Client-%d] Failed to set read deadline: %v", s.id, err)
			netutil.SafeWrite(errChan, fmt.Errorf("failed to set read deadline: %w", err))
			return
		}
		// Read into buf[datagramHeaderLen:] so the stripped datagram payload lands in canonical
		// position: buf[datagramHeaderLen] is the type prefix, buf[tunReserve:] the IP packet.
		n, err := conn.Read(buf[datagramHeaderLen:])
		if err != nil {
			config.LPool.Put(buf[:])
			plog.G(ctx).Errorf("[Client-%d] Failed to read from remote: %v", s.id, err)
			netutil.SafeWrite(errChan, fmt.Errorf("failed to read packet from remote %s: %w", conn.RemoteAddr(), err))
			return
		}
		if n < 1 {
			config.LPool.Put(buf[:])
			continue
		}
		ip := buf[tunReserve : datagramHeaderLen+n]
		logIPPacket(ctx, fmt.Sprintf("[Client-%d] INBOUND", s.id), ip)
		if buf[datagramHeaderLen] == packetTypeToGvisor {
			gvisorInbound <- NewPacket(buf[:], n, nil, nil)
		} else {
			// Heartbeat echo replies from the gateway are the data-plane liveness signal:
			// record them and drop (the OS would discard them — the echo was crafted by us).
			if s.stats != nil && isHeartbeatEchoReply(ip) {
				s.stats.MarkReply()
				config.LPool.Put(buf[:])
				continue
			}
			s.tunOutbound <- NewPacket(buf[:], n, nil, nil)
		}
	}
}

// writeToConn writes packets from this slot's inbound channel to the raw TCP conn with
// datagram framing.
func (s *connSlot) writeToConn(ctx context.Context, rawConn net.Conn, errChan chan error) {
	defer netutil.HandleCrash()
	dl := &periodicDeadline{timeout: config.KeepAliveTime, set: rawConn.SetWriteDeadline}
	for {
		select {
		case packet := <-s.inbound:
			if packet == nil {
				return
			}
			if err := dl.refresh(); err != nil {
				plog.G(ctx).Errorf("[Client-%d] Failed to set write deadline: %v", s.id, err)
				netutil.SafeWrite(errChan, fmt.Errorf("failed to set write deadline: %w", err))
				return
			}
			// Write datagram frame in-place: [2-byte length header][prefix+IP data]
			binary.BigEndian.PutUint16(packet.data[:datagramHeaderLen], uint16(packet.length))
			_, err := rawConn.Write(packet.data[:packet.length+datagramHeaderLen])
			packet.release()
			if err != nil {
				plog.G(ctx).Errorf("[Client-%d] Failed to write to remote: %v", s.id, err)
				netutil.SafeWrite(errChan, fmt.Errorf("failed to write packet to remote: %w", err))
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// periodicDeadline refreshes a connection deadline lazily — only once less than half the
// timeout remains — to avoid a SetDeadline syscall on every packet. The first refresh() always
// sets the deadline.
type periodicDeadline struct {
	next    time.Time
	timeout time.Duration
	set     func(time.Time) error
}

func (p *periodicDeadline) refresh() error {
	if !p.next.IsZero() && time.Until(p.next) >= p.timeout/2 {
		return nil
	}
	p.next = time.Now().Add(p.timeout)
	return p.set(p.next)
}

// registrationPayloads builds the proactive route-registration payloads — one ICMP echo to the
// gateway per TUN address family, each prefixed with the gvisor type byte (canonical layout, no
// datagram length header: UDPConnOverTCP.Write frames that on send). A slot writes these on every
// (re)connect so the server registers the route for that conn immediately. The echo also doubles
// as a liveness ping (its reply marks HeartbeatStats). Returns nil if the TUN IPs are unavailable.
func (t *clientTransport) registrationPayloads() [][]byte {
	tunIfi, err := netutil.GetTunDeviceByConn(t.dev.tun)
	if err != nil {
		return nil
	}
	srcIPv4, srcIPv6, _, _ := netutil.GetTunDeviceIP(tunIfi.Name)
	var payloads [][]byte
	appendPayload := func(icmp []byte) {
		payload := make([]byte, typePrefixLen+len(icmp))
		payload[0] = packetTypeToGvisor
		copy(payload[typePrefixLen:], icmp)
		payloads = append(payloads, payload)
	}
	if srcIPv4 != nil {
		if icmp, e := netutil.GenICMPPacket(srcIPv4, config.RouterIP); e == nil {
			appendPayload(icmp)
		}
	}
	if srcIPv6 != nil {
		if icmp, e := netutil.GenICMPPacketIPv6(srcIPv6, config.RouterIP6); e == nil {
			appendPayload(icmp)
		}
	}
	return payloads
}

func (t *clientTransport) heartbeats(ctx context.Context) {
	tunIfi, err := netutil.GetTunDeviceByConn(t.dev.tun)
	if err != nil {
		plog.G(ctx).Errorf("[Client] Failed to get tun device: %v", err)
		return
	}

	ticker := time.NewTicker(config.KeepAliveTime)
	defer ticker.Stop()

	sendHeartbeat := func(payload []byte) {
		buf := config.LPool.Get().([]byte)
		n := copy(buf[tunReserve:], payload)
		buf[datagramHeaderLen] = packetTypeToGvisor
		t.dev.tunInbound <- NewPacket(buf, n+typePrefixLen, nil, nil)
	}

	sendAll := func(reason string) {
		srcIPv4, srcIPv6, dockerSrcIPv4, _ := netutil.GetTunDeviceIP(tunIfi.Name)
		plog.G(ctx).Debugf("[Client] Sending heartbeat (%s)", reason)
		if srcIPv4 != nil {
			if icmp, e := netutil.GenICMPPacket(srcIPv4, config.RouterIP); e != nil {
				plog.G(ctx).Errorf("[Client] Failed to generate IPv4 heartbeat: %v", e)
			} else {
				sendHeartbeat(icmp)
			}
		}
		if srcIPv6 != nil {
			if icmp, e := netutil.GenICMPPacketIPv6(srcIPv6, config.RouterIP6); e != nil {
				plog.G(ctx).Errorf("[Client] Failed to generate IPv6 heartbeat: %v", e)
			} else {
				sendHeartbeat(icmp)
			}
		}
		if dockerSrcIPv4 != nil {
			_, _ = netutil.Ping(ctx, dockerSrcIPv4.String(), config.DockerRouterIP.String())
		}
	}

	sendAll("initial")
	for ctx.Err() == nil {
		select {
		case <-ticker.C:
			sendAll("periodic")
		case <-ctx.Done():
			return
		}
	}
}
