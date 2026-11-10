package tunnel

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	framing "github.com/wencaiwulue/kubevpn/v2/pkg/internal/wire"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
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
	role := a.role.String()
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

	if a.role.isControl() {
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
				dataPlaneWarn.Load().Warnf(ctx, "reg-write",
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
