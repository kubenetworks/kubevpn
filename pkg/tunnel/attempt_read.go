package tunnel

import (
	"context"
	"fmt"
	"net"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

// readGeneration reads datagram-framed packets from the server, routing IP packets to the TUN and
// inter-client packets to the transport-level shared gVisor stack. conn is the framed
// *framing.Conn in production (typed as net.Conn so tests can drive it with a raw pipe).
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
