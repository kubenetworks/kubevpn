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
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

const (
	// ConnPoolSize is the number of parallel TCP connections to the server.
	// Each connection handles a subset of traffic (partitioned by dst IP hash).
	// Multiple connections reduce head-of-line blocking and improve throughput.
	ConnPoolSize = 4
)

// clientTransport is the spoke/endpoint half of the tun device. Outbound packets read from the
// TUN are distributed across a pool of dialed connections to the server (by dst IP hash), or
// looped back through a local gvisor stack when src == dst. Inbound packets from the server are
// delivered to the device's tunOutbound by the per-connection readers.
type clientTransport struct {
	dev     *tunDevice
	forward *Forwarder
	// slots holds the per-connection pool; built by runConnPool, read by routeOutbound.
	slots []*connSlot
	// reconnected signals the heartbeat goroutine to send an immediate heartbeat after a
	// connection is (re)established, so the server registers the route without waiting a cycle.
	reconnected chan struct{}
	// gvisorInbound carries src == dst packets to the local loopback gvisor stack.
	gvisorInbound chan *Packet
	// stats records data-plane liveness from observed heartbeat echo replies; may be nil.
	stats *HeartbeatStats
}

func newClientTransport(dev *tunDevice, forward *Forwarder, stats *HeartbeatStats) *clientTransport {
	return &clientTransport{
		dev:           dev,
		forward:       forward,
		reconnected:   make(chan struct{}, 1),
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
// stack when src == dst, otherwise enqueue it for the connection pool (distributed by dst hash).
func (t *clientTransport) routeOutbound(ctx context.Context, buf []byte, n int, src, dst net.IP) {
	// buf is canonical (pumpTun reserved buf[0:tunReserve]): set the type prefix and the
	// IP already sits at buf[tunReserve:]. Both branches forward the same canonical buffer.
	buf[datagramHeaderLen] = 1
	logIPPacket(ctx, "[Client] OUTBOUND", buf[tunReserve:tunReserve+n])
	if src.Equal(dst) {
		t.gvisorInbound <- NewPacket(buf[:], n+typePrefixLen, src, dst)
	} else {
		// Enqueue to the shared tunInbound; runConnPool distributes to a slot by hash(dst).
		t.dev.tunInbound <- NewPacket(buf[:], n+typePrefixLen, src, dst)
	}
}

// runConnPool creates N parallel connection slots and distributes packets by dst IP hash. Each
// slot runs independently — if one connection breaks, only that slot reconnects. It waits for all
// slot goroutines to exit before returning, for deterministic shutdown.
func (t *clientTransport) runConnPool(ctx context.Context) {
	n := ConnPoolSize
	t.slots = make([]*connSlot, n)
	var wg sync.WaitGroup
	for i := range t.slots {
		slot := &connSlot{
			id:          i,
			inbound:     make(chan *Packet, MaxSize),
			tunOutbound: t.dev.tunOutbound,
			forward:     t.forward,
			stats:       t.stats,
		}
		// Only slot 0 signals reconnects to the heartbeat goroutine.
		if i == 0 {
			slot.reconnected = t.reconnected
		}
		t.slots[i] = slot
		wg.Add(1)
		go func(s *connSlot) {
			defer wg.Done()
			defer util.HandleCrash()
			s.run(ctx)
		}(slot)
	}
	defer wg.Wait()

	// Drain the shared tunInbound and distribute to slots by dst IP hash.
	// Heartbeats (dst == nil) are broadcast to ALL slots to keep idle connections alive.
	for {
		select {
		case packet := <-t.dev.tunInbound:
			if packet == nil {
				return
			}
			if packet.dst != nil {
				trySendToSlot(t.slots[ipHash(packet.dst, n)].inbound, packet)
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

// connSlot is one connection in the client pool. It owns its inbound packet channel and the
// dial/reconnect lifecycle, and runs the framed read/write loops over the connection. It
// replaces the previous slotID-threaded free functions.
type connSlot struct {
	id          int
	inbound     chan *Packet
	tunOutbound chan *Packet
	forward     *Forwarder
	// reconnected is non-nil only for slot 0; it signals the heartbeat goroutine to send an
	// immediate heartbeat after a reconnect so the server re-registers the route promptly.
	reconnected chan struct{}
	// stats records data-plane liveness from observed heartbeat echo replies; may be nil.
	stats *HeartbeatStats
}

// run manages this slot's single connection with reconnect logic.
func (s *connSlot) run(ctx context.Context) {
	firstConnect := true
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

			// Signal heartbeat on reconnect (not first connect — initial heartbeat covers that).
			if s.reconnected != nil && !firstConnect {
				select {
				case s.reconnected <- struct{}{}:
				default:
				}
			}
			firstConnect = false

			udpConn := conn.(*UDPConnOverTCP)
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
	defer util.HandleCrash()
	gvisorInbound := make(chan *Packet, MaxSize)
	go handleGvisorPacket(gvisorInbound, s.inbound, datagramHeaderLen).Run(ctx)
	dl := &periodicDeadline{timeout: config.KeepAliveTime * 3, set: conn.SetReadDeadline}
	for ctx.Err() == nil {
		buf := config.LPool.Get().([]byte)[:]
		if err := dl.refresh(); err != nil {
			config.LPool.Put(buf[:])
			plog.G(ctx).Errorf("[Client-%d] Failed to set read deadline: %v", s.id, err)
			util.SafeWrite(errChan, fmt.Errorf("failed to set read deadline: %w", err))
			return
		}
		// Read into buf[datagramHeaderLen:] so the stripped datagram payload lands in canonical
		// position: buf[datagramHeaderLen] is the type prefix, buf[tunReserve:] the IP packet.
		n, err := conn.Read(buf[datagramHeaderLen:])
		if err != nil {
			config.LPool.Put(buf[:])
			plog.G(ctx).Errorf("[Client-%d] Failed to read from remote: %v", s.id, err)
			util.SafeWrite(errChan, fmt.Errorf("failed to read packet from remote %s: %w", conn.RemoteAddr(), err))
			return
		}
		if n < 1 {
			config.LPool.Put(buf[:])
			continue
		}
		ip := buf[tunReserve : datagramHeaderLen+n]
		logIPPacket(ctx, fmt.Sprintf("[Client-%d] INBOUND", s.id), ip)
		if buf[datagramHeaderLen] == 1 {
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
	defer util.HandleCrash()
	dl := &periodicDeadline{timeout: config.KeepAliveTime, set: rawConn.SetWriteDeadline}
	for {
		select {
		case packet := <-s.inbound:
			if packet == nil {
				return
			}
			if err := dl.refresh(); err != nil {
				plog.G(ctx).Errorf("[Client-%d] Failed to set write deadline: %v", s.id, err)
				util.SafeWrite(errChan, fmt.Errorf("failed to set write deadline: %w", err))
				return
			}
			// Write datagram frame in-place: [2-byte length header][prefix+IP data]
			binary.BigEndian.PutUint16(packet.data[:datagramHeaderLen], uint16(packet.length))
			_, err := rawConn.Write(packet.data[:packet.length+datagramHeaderLen])
			packet.release()
			if err != nil {
				plog.G(ctx).Errorf("[Client-%d] Failed to write to remote: %v", s.id, err)
				util.SafeWrite(errChan, fmt.Errorf("failed to write packet to remote: %w", err))
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

func (t *clientTransport) heartbeats(ctx context.Context) {
	tunIfi, err := util.GetTunDeviceByConn(t.dev.tun)
	if err != nil {
		plog.G(ctx).Errorf("[Client] Failed to get tun device: %v", err)
		return
	}

	ticker := time.NewTicker(config.KeepAliveTime)
	defer ticker.Stop()

	sendHeartbeat := func(payload []byte) {
		buf := config.LPool.Get().([]byte)
		n := copy(buf[tunReserve:], payload)
		buf[datagramHeaderLen] = 1
		t.dev.tunInbound <- NewPacket(buf, n+typePrefixLen, nil, nil)
	}

	sendAll := func(reason string) {
		srcIPv4, srcIPv6, dockerSrcIPv4, _ := util.GetTunDeviceIP(tunIfi.Name)
		plog.G(ctx).Debugf("[Client] Sending heartbeat (%s)", reason)
		if srcIPv4 != nil {
			if icmp, e := util.GenICMPPacket(srcIPv4, config.RouterIP); e != nil {
				plog.G(ctx).Errorf("[Client] Failed to generate IPv4 heartbeat: %v", e)
			} else {
				sendHeartbeat(icmp)
			}
		}
		if srcIPv6 != nil {
			if icmp, e := util.GenICMPPacketIPv6(srcIPv6, config.RouterIP6); e != nil {
				plog.G(ctx).Errorf("[Client] Failed to generate IPv6 heartbeat: %v", e)
			} else {
				sendHeartbeat(icmp)
			}
		}
		if dockerSrcIPv4 != nil {
			_, _ = util.Ping(ctx, dockerSrcIPv4.String(), config.DockerRouterIP.String())
		}
	}

	sendAll("initial")
	for ctx.Err() == nil {
		select {
		case <-ticker.C:
			sendAll("periodic")
		case <-t.reconnected:
			sendAll("reconnected")
			ticker.Reset(config.KeepAliveTime)
		case <-ctx.Done():
			return
		}
	}
}
