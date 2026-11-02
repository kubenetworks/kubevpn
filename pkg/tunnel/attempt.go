package tunnel

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	framing "github.com/wencaiwulue/kubevpn/v2/pkg/internal/wire"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

// connAttempt borrows the slot queues but owns exactly one dial, handshake and
// pair of pumps. run joins them before the slot may consume its queue again.
type connAttempt struct {
	*connSlot
	now                          func() time.Time
	connectedAt, failedAt        time.Time
	failureOnce                  sync.Once
	dialFailure, postDialFailure bool
	conn                         net.Conn
	cancel                       context.CancelFunc
	loops                        sync.WaitGroup
	directWriteStage             *datapathStage
}

func (a *connAttempt) observeFailure() {
	if a.now != nil {
		a.failureOnce.Do(func() { a.failedAt = a.now() })
	}
}

func (a *connAttempt) run(ctx context.Context, generation uint64) {

	subCtx, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	defer cancel()

	// Fence each underlying connection, not this long-lived slot. A
	// generation change during Dial discards that connection and retries.
	role := "data"
	if a.isControl {
		role = "control"
	}
	a.directWriteStage = diagnosticStage(subCtx, "socket_write", role, a.id, generation)
	defer a.directWriteStage.close()
	conn, err := a.forward.DialContext(subCtx)
	if err != nil {
		a.dialFailure = true
		a.stats.slotTransition(a.id, generation, false, "dial: "+err.Error())
		plog.G(ctx).Errorf("[Client-%d] Failed to dial %s: %v", a.id, a.forward.Addr, err)
		return
	}
	a.conn = conn
	a.connectedAt = a.now()

	owner := conn
	owner.TakeOwnership()
	defer owner.Close()
	closed := make(chan struct{})
	stopClose := context.AfterFunc(subCtx, func() { defer close(closed); _ = owner.Close() })
	defer func() {
		if !stopClose() {
			<-closed
		}
	}()
	if generation != a.stats.Generation() {
		return
	}
	a.stats.slotTransition(a.id, generation, true, "")
	defer a.stats.slotTransition(a.id, generation, false, "")
	plog.G(ctx).Debugf("[Client-%d] Connected generation=%d to %s", a.id, generation, conn.RemoteAddr())
	rawConn := owner.Conn
	udpConn := framing.NewView(subCtx, &slotFailureConn{Conn: rawConn, close: owner.Close}, a.observeFailure)

	if a.isControl {
		// Control conn: send a packetTypeControl declaration so the server enters
		// the control loop (no AddRoute, no Router registration).
		controlDecl := []byte{packetTypeControl}
		if _, err = a.writePayload(udpConn, controlDecl, generation, false); err != nil {
			a.observeFailure()
			a.postDialFailure = true
			a.stats.slotTransition(a.id, generation, false, "control declaration: "+err.Error())
			plog.G(ctx).Debugf("[Client-%d] Failed to send control declaration: %v", a.id, err)
			return
		}
	} else if a.registrations != nil {
		// Data conn: announce our TUN IP(s) so the server registers the route immediately.
		for _, payload := range a.registrations(ctx) {
			if _, err = a.writePayload(udpConn, payload, generation, true); err != nil {
				a.observeFailure()
				a.postDialFailure = true
				// Not cosmetic: without this announcement the server keeps no route for us
				// and silently drops everything addressed to us, heartbeat replies included.
				dataPlaneWarn.Warnf(ctx, "reg-write",
					"[Client-%d] Failed to announce our route on connect: %v; the server will drop "+
						"inbound traffic for us until a later attempt succeeds", a.id, err)
				a.stats.slotTransition(a.id, generation, false, "registration: "+err.Error())
				return
			}
		}
	}

	errChan := make(chan error, 2)
	loops := &a.loops
	loops.Add(2)
	go func() {
		defer loops.Done()
		a.readGeneration(subCtx, udpConn, errChan, generation)
	}()
	go func() {
		defer loops.Done()
		a.writeGeneration(subCtx, rawConn, errChan, generation)
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
		a.observeFailure()
		a.postDialFailure = true
		a.stats.slotTransition(a.id, generation, false, err.Error())
		plog.G(ctx).Warnf("[Client-%d] Disconnected generation=%d from %s: %v", a.id, generation, conn.RemoteAddr(), err)
	case <-ctx.Done():
		return
	}
}

// slotFailureConn timestamps framing faults before framing.Conn closes the
// damaged stream. The batch writer still uses the original raw connection.
// Close delegates to the original framed owner, preserving its close-once policy.
type slotFailureConn struct {
	net.Conn
	close func() error
}

func (c *slotFailureConn) Close() error {
	return c.close()
}

// readFromConn reads datagram-framed packets from the server, routing IP packets to the tun and
// inter-client (gvisor-prefixed) packets to the transport-level shared gvisor stack via
// s.interClientInbound. conn is the framed *framing.Conn in production (typed as net.Conn so
// tests can drive it with a raw pipe).
func (s *connAttempt) readGeneration(ctx context.Context, conn net.Conn, errChan chan error, generation uint64) {
	inject := diagnosticStage(ctx, "gvisor_inject", "data", s.id, generation)
	defer inject.close()
	enqueue := diagnosticStage(ctx, "tun_out_enqueue", "data", s.id, generation)
	defer enqueue.close()
	defer netutil.HandleCrash()
	dl := &periodicDeadline{timeout: config.KeepAliveTime * 3, set: conn.SetReadDeadline}
	for ctx.Err() == nil {
		buf := config.LPool.Get().([]byte)[:]
		if err := dl.refresh(); err != nil {
			if s.now != nil {
				s.observeFailure()
			}
			config.LPool.Put(buf[:])
			plog.G(ctx).Errorf("[Client-%d] Failed to set read deadline: %v", s.id, err)
			netutil.SafeWrite(errChan, fmt.Errorf("failed to set read deadline: %w", err))
			return
		}
		// Read into buf[datagramHeaderLen:] so the stripped datagram payload lands in canonical
		// position: buf[datagramHeaderLen] is the type prefix, buf[tunReserve:] the IP packet.
		n, err := conn.Read(buf[datagramHeaderLen:])
		if err != nil {
			if s.now != nil {
				s.observeFailure()
			}
			config.LPool.Put(buf[:])
			plog.G(ctx).Errorf("[Client-%d] Failed to read from remote: %v", s.id, err)
			netutil.SafeWrite(errChan, fmt.Errorf("failed to read packet from remote %s: %w", conn.RemoteAddr(), err))
			return
		}
		if n < 1 {
			config.LPool.Put(buf[:])
			continue
		}
		s.stats.slotActivity(s.id, generation, true, 1, n)
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
				token := inject.begin()
				s.interClient.InjectIP(ip)
				inject.finish(token, 1, len(ip), ctx.Err() == nil)
			}
			config.LPool.Put(buf[:])
		} else {
			// Only this session's correlated echoes renew health. Protocol-owned
			// stale echoes are dropped; application gateway pings reach the TUN.
			if s.stats.observeReply(ip, generation, s.id) {
				config.LPool.Put(buf[:])
				continue
			}
			packet := newPacket(buf[:], n, nil, nil)
			if !diagnosticEnqueue(ctx, s.tunOutbound, packet, enqueue) {
				packet.release()
				return
			}
		}
	}
}

// writeToConn writes packets from this slot's inbound channel to the raw TCP conn with
// datagram framing.
// writePayload handles direct registration writes using the same correlation
// contract as queued control probes. The framed connection owns framing here.
func (s *connAttempt) writePayload(conn net.Conn, payload []byte, generation uint64, probe bool) (int, error) {
	var id uint64
	if probe && len(payload) > typePrefixLen {
		ip, token := s.stats.prepareProbe(payload[typePrefixLen:], generation, s.id)
		if ip == nil {
			if s.now != nil {
				s.observeFailure()
			}
			return 0, fmt.Errorf("retired heartbeat generation")
		}
		id = token
		payload = append([]byte{payload[0]}, ip...)
	}
	if err := conn.SetWriteDeadline(time.Now().Add(config.KeepAliveTime)); err != nil {
		if s.now != nil {
			s.observeFailure()
		}
		s.stats.finishProbe(id, false)
		return 0, err
	}
	token := s.directWriteStage.begin(1, len(payload))
	n, err := conn.Write(payload)
	if err != nil && s.now != nil {
		s.observeFailure()
	}
	s.directWriteStage.finish(token, 1, n, err == nil && n == len(payload))
	s.stats.finishProbe(id, err == nil && n == len(payload))
	if err == nil {
		s.stats.slotActivity(s.id, generation, false, 1, n)
	}
	return n, err
}

func (s *connAttempt) writeGeneration(ctx context.Context, rawConn net.Conn, errChan chan error, generation uint64) {
	role := "data"
	if s.isControl {
		role = "control"
	}
	stage := diagnosticStage(ctx, "socket_write", role, s.id, generation)
	defer stage.close()
	stage.setQueue(s.inbound)
	defer netutil.HandleCrash()
	dl := &periodicDeadline{timeout: config.KeepAliveTime, set: rawConn.SetWriteDeadline}
	// readDL keeps this slot's read (liveness) deadline alive on write activity. A slot busy
	// sending bulk data is not dead, even when the reverse traffic (ACKs) for the flow it carries
	// is routed to a different pool slot (server RouteMapTCP is last-writer-wins, and heartbeats
	// broadcast on every slot make it flap). Without this, readFromConn's KeepAliveTime*3 read
	// timeout can tear down an actively-sending slot mid-transfer, destroying the per-slot gvisor
	// stack it hosts (bound to subCtx) and aborting the transfer. framing.Conn embeds this same
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
		for _, p := range batch.packets[:batch.n] {
			s.stats.dequeueProbe(p)
		}
		if err := dl.refresh(); err != nil {
			if s.now != nil {
				s.observeFailure()
			}
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
		token := stage.begin(batch.n, batchDiagnosticBytes(batch.packets[:batch.n]))
		complete, err := writer.write(rawConn, batch.packets[:batch.n])
		if err != nil && s.now != nil {
			s.observeFailure()
		}
		stage.finish(token, complete, batchDiagnosticBytes(batch.packets[:complete]), err == nil && ctx.Err() == nil)
		s.stats.finishProbe(probeID, err == nil && complete == batch.n)
		if s.stats != nil && complete > 0 {
			bytes := 0
			for _, p := range batch.packets[:complete] {
				bytes += p.length
			}
			s.stats.slotActivity(s.id, generation, false, complete, bytes)
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
