package core

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

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
	// interClientInbound is the transport-level channel feeding the shared inter-client gvisor
	// stack. readFromConn routes type == packetTypeToGvisor packets here instead of to a per-slot
	// stack, so the stack outlives any single slot. nil in tests that drive a slot in isolation.
	interClientInbound chan *Packet
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

// readFromConn reads datagram-framed packets from the server, routing IP packets to the tun and
// inter-client (gvisor-prefixed) packets to the transport-level shared gvisor stack via
// s.interClientInbound. conn is the framed *UDPConnOverTCP in production (typed as net.Conn so
// tests can drive it with a raw pipe).
func (s *connSlot) readFromConn(ctx context.Context, conn net.Conn, errChan chan error) {
	defer netutil.HandleCrash()
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
			// Hand off to the transport-level inter-client stack. Drop-if-full (and nil-safe for
			// tests that drive a slot in isolation) so a busy stack cannot stall this slot's reader.
			trySendToSlot(s.interClientInbound, NewPacket(buf[:], n, nil, nil))
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
	// readDL keeps this slot's read (liveness) deadline alive on write activity. A slot busy
	// sending bulk data is not dead, even when the reverse traffic (ACKs) for the flow it carries
	// is routed to a different pool slot (server RouteMapTCP is last-writer-wins, and heartbeats
	// broadcast on every slot make it flap). Without this, readFromConn's KeepAliveTime*3 read
	// timeout can tear down an actively-sending slot mid-transfer, destroying the per-slot gvisor
	// stack it hosts (bound to subCtx) and aborting the transfer. UDPConnOverTCP embeds this same
	// conn, so both goroutines push the one underlying read deadline forward (SetReadDeadline is
	// safe to call concurrently with an in-flight Read). Liveness is thus "active in either
	// direction"; a slot idle in BOTH directions still times out via readFromConn.
	readDL := &periodicDeadline{timeout: config.KeepAliveTime * 3, set: rawConn.SetReadDeadline}
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
			// A successful write proves this slot is alive: push the read/liveness deadline forward.
			// Best-effort — the write already succeeded, so a rare SetReadDeadline hiccup should not
			// tear down the slot; a genuinely dead conn is caught by the next write or by readFromConn.
			if err := readDL.refresh(); err != nil {
				plog.G(ctx).Debugf("[Client-%d] Failed to refresh read deadline after write: %v", s.id, err)
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
