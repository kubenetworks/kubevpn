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
	registrations func(ctx context.Context) [][]byte
	// interClient is the shared transport-level inter-client gvisor stack. readFromConn injects
	// type == packetTypeToGvisor packets directly into it (InjectIP) rather than into a per-slot
	// stack, so the stack outlives any single slot. nil in tests that drive a slot in isolation.
	interClient *interClientStack
	// isControl marks this slot as the dedicated control-plane connection. On connect, it sends
	// a packetTypeControl declaration so the server handles it separately (no RouteHub registration).
	isControl bool
}

// run manages this slot's single connection with reconnect logic.
func (s *connSlot) run(ctx context.Context) {
	for ctx.Err() == nil {
		func() {
			subCtx, cancel := context.WithCancel(ctx)
			defer cancel()

			// Fence each underlying connection, not this long-lived slot. A
			// generation change during Dial discards that connection and retries.
			generation := s.stats.Generation()
			conn, err := s.forward.DialContext(subCtx)
			if err != nil {
				s.stats.slotTransition(s.id, generation, false, "dial: "+err.Error())
				plog.G(ctx).Errorf("[Client-%d] Failed to dial %s: %v", s.id, s.forward.Addr, err)
				select {
				case <-time.After(config.SlotReconnectBackoff):
				case <-ctx.Done():
				}
				return
			}
			defer conn.Close()
			stopClose := context.AfterFunc(subCtx, func() { _ = conn.Close() })
			defer stopClose()
			if generation != s.stats.Generation() {
				return
			}
			s.stats.slotTransition(s.id, generation, true, "")
			defer s.stats.slotTransition(s.id, generation, false, "")
			plog.G(ctx).Debugf("[Client-%d] Connected generation=%d to %s", s.id, generation, conn.RemoteAddr())

			udpConn := conn.(*UDPConnOverTCP)

			if s.isControl {
				// Control conn: send a packetTypeControl declaration so the server enters
				// the control loop (no AddRoute, no RouteHub registration).
				controlDecl := []byte{packetTypeControl}
				if _, err = s.writePayload(udpConn, controlDecl, generation, false); err != nil {
					s.stats.slotTransition(s.id, generation, false, "control declaration: "+err.Error())
					plog.G(ctx).Debugf("[Client-%d] Failed to send control declaration: %v", s.id, err)
					return
				}
			} else if s.registrations != nil {
				// Data conn: announce our TUN IP(s) so the server registers the route immediately.
				for _, payload := range s.registrations(ctx) {
					if _, err = s.writePayload(udpConn, payload, generation, true); err != nil {
						// Not cosmetic: without this announcement the server keeps no route for us
						// and silently drops everything addressed to us, heartbeat replies included.
						dataPlaneWarn.Warnf(ctx, "reg-write",
							"[Client-%d] Failed to announce our route on connect: %v; the server will drop "+
								"inbound traffic for us until a later attempt succeeds", s.id, err)
						s.stats.slotTransition(s.id, generation, false, "registration: "+err.Error())
						return
					}
				}
			}

			errChan := make(chan error, 2)
			var loops sync.WaitGroup
			loops.Add(2)
			go func() {
				defer loops.Done()
				s.readGeneration(subCtx, udpConn, errChan, generation)
			}()
			go func() {
				defer loops.Done()
				s.writeGeneration(subCtx, udpConn.Conn, errChan, generation)
			}()
			defer func() {
				// Unblock socket I/O and join both consumers before another dial
				// iteration can start a writer on this slot's shared inbound queue.
				cancel()
				_ = conn.Close()
				loops.Wait()
			}()

			select {
			case err = <-errChan:
				s.stats.slotTransition(s.id, generation, false, err.Error())
				plog.G(ctx).Warnf("[Client-%d] Disconnected generation=%d from %s: %v", s.id, generation, conn.RemoteAddr(), err)
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
	s.readGeneration(ctx, conn, errChan, s.stats.Generation())
}

func (s *connSlot) readGeneration(ctx context.Context, conn net.Conn, errChan chan error, generation uint64) {
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
		s.stats.slotActivity(s.id, generation, true, n)
		ip := buf[tunReserve : datagramHeaderLen+n]
		if plog.IsDebugEnabled(ctx) {
			logIPPacket(ctx, fmt.Sprintf("[Client-%d] INBOUND", s.id), ip)
		}
		if buf[datagramHeaderLen] == packetTypeToGvisor {
			// Inject directly into the shared inter-client stack, mirroring the server's
			// readFromTCPConnWriteToEndpoint: no intermediate bounded queue, so TCP's receive
			// window throttles the peer instead of a blind drop that forces sender retransmits.
			// InjectIP returns promptly (the localhost copy runs in the forwarder's goroutines),
			// so a busy stack never stalls this reader — heartbeat-reply liveness is preserved.
			// nil-safe for tests that drive a slot in isolation.
			if s.interClient != nil {
				s.interClient.InjectIP(ip)
			}
			config.LPool.Put(buf[:])
		} else {
			// Only this session's correlated echoes renew health. Protocol-owned
			// stale echoes are dropped; application gateway pings reach the TUN.
			if s.stats.observeReply(ip, generation, s.id) {
				config.LPool.Put(buf[:])
				continue
			}
			select {
			case s.tunOutbound <- NewPacket(buf[:], n, nil, nil):
			case <-ctx.Done():
				config.LPool.Put(buf[:])
				return
			}
		}
	}
}

// writeToConn writes packets from this slot's inbound channel to the raw TCP conn with
// datagram framing.
func (s *connSlot) writeToConn(ctx context.Context, rawConn net.Conn, errChan chan error) {
	s.writeGeneration(ctx, rawConn, errChan, s.stats.Generation())
}

// writePayload handles direct registration writes using the same correlation
// contract as queued control probes. The framed connection owns framing here.
func (s *connSlot) writePayload(conn net.Conn, payload []byte, generation uint64, probe bool) (int, error) {
	var id uint64
	if probe && len(payload) > typePrefixLen {
		ip, token := s.stats.prepareProbe(payload[typePrefixLen:], generation, s.id)
		if ip == nil {
			return 0, fmt.Errorf("retired heartbeat generation")
		}
		id = token
		payload = append([]byte{payload[0]}, ip...)
	}
	if err := conn.SetWriteDeadline(time.Now().Add(config.KeepAliveTime)); err != nil {
		s.stats.finishProbe(id, false)
		return 0, err
	}
	n, err := conn.Write(payload)
	s.stats.finishProbe(id, err == nil && n == len(payload))
	if err == nil {
		s.stats.slotActivity(s.id, generation, false, n)
	}
	return n, err
}

func (s *connSlot) writeGeneration(ctx context.Context, rawConn net.Conn, errChan chan error, generation uint64) {
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
	var batch packetBatch
	var writer frameBatchWriter
	defer batch.close()
	limit := maxBatchFrames
	if s.isControl {
		// Stamp correlated probes immediately before their own write, never while
		// collecting a batch that could delay the probe's actual send time.
		limit = 1
	}
	for batch.next(ctx, s.inbound, limit, nil) {
		if err := dl.refresh(); err != nil {
			netutil.SafeWrite(errChan, fmt.Errorf("failed to set write deadline: %w", err))
			return
		}
		var probeID uint64
		packet := batch.packets[0]
		if s.isControl && packet.data[datagramHeaderLen] == packetTypeControl {
			ip, id := s.stats.prepareProbe(packet.data[tunReserve:datagramHeaderLen+packet.length], generation, s.id)
			if ip == nil {
				batch.release()
				continue
			}
			probeID = id
			packet.length = copy(packet.data[tunReserve:], ip) + typePrefixLen
		}
		for _, p := range batch.packets[:batch.n] {
			binary.BigEndian.PutUint16(p.data[:datagramHeaderLen], uint16(p.length))
		}
		complete, err := writer.write(rawConn, batch.packets[:batch.n])
		s.stats.finishProbe(probeID, err == nil && complete == batch.n)
		for _, p := range batch.packets[:complete] {
			s.stats.slotActivity(s.id, generation, false, p.length)
		}
		batch.release()
		if err != nil {
			plog.G(ctx).Errorf("[Client-%d] Failed to write to remote: %v", s.id, err)
			netutil.SafeWrite(errChan, fmt.Errorf("failed to write packet to remote: %w", err))
			return
		}
		// A completed batch proves this slot is alive. Refresh best-effort.
		if err := readDL.refresh(); err != nil {
			plog.G(ctx).Debugf("[Client-%d] Failed to refresh read deadline after write: %v", s.id, err)
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
// channel is full so a slow slot cannot stall the others. Returns false if the packet was dropped.
func trySendToSlot(inbound chan *Packet, packet *Packet) bool {
	select {
	case inbound <- packet:
		return true
	default:
		packet.release()
		return false
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
