package tunnel

import (
	"context"
	"fmt"
	"net"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

// readGeneration owns connection deadlines and read errors. Each packet is
// consumed by dispatchReadPacket before the next datagram is read.
func (s *connAttempt) readGeneration(ctx context.Context, conn net.Conn, errChan chan error, generation uint64) {
	inject := diagnosticStage(ctx, "gvisor_inject", "data", s.id, generation)
	defer inject.close()
	enqueue := diagnosticStage(ctx, "tun_out_enqueue", "data", s.id, generation)
	defer enqueue.close()
	defer netutil.HandleCrash()
	dl := &periodicDeadline{timeout: config.KeepAliveTime * 3, set: conn.SetReadDeadline}
	reader := packetReader{conn: conn}
	for ctx.Err() == nil {
		if err := dl.refresh(); err != nil {
			if s.now != nil {
				s.observeFailure()
			}
			plog.G(ctx).Errorf("[Client-%d] Failed to set read deadline: %v", s.id, err)
			netutil.SafeWrite(errChan, fmt.Errorf("failed to set read deadline: %w", err))
			return
		}
		packet, err := reader.read()
		if err != nil {
			if s.now != nil {
				s.observeFailure()
			}
			plog.G(ctx).Errorf("[Client-%d] Failed to read from remote: %v", s.id, err)
			netutil.SafeWrite(errChan, fmt.Errorf("failed to read packet from remote %s: %w", conn.RemoteAddr(), err))
			return
		}
		if !s.dispatchReadPacket(ctx, packet, generation, inject, enqueue) {
			return
		}
	}
}

// dispatchReadPacket consumes packet, either releasing it or transferring its
// owning reference to tunOutbound. It returns false when the read loop must end.
func (s *connAttempt) dispatchReadPacket(ctx context.Context, packet *packet, generation uint64, inject, enqueue *datapathStage) bool {
	n := packet.length
	if n < 1 {
		packet.release()
		return true
	}
	s.stats.slotActivity(s.id, generation, true, 1, n)
	ip := packet.ip()
	if plog.IsDebugEnabled(ctx) {
		logIPPacket(ctx, fmt.Sprintf("[Client-%d] INBOUND", s.id), ip)
	}
	if packet.kind() == packetKindToGvisor {
		// Inject directly into the shared inter-client stack, mirroring the server's
		// dispatch: no intermediate bounded queue, so TCP's receive window throttles
		// the peer instead of a blind drop that forces sender retransmits.
		// nil-safe for tests that drive a slot in isolation.
		if s.interClient != nil {
			token := inject.begin()
			s.interClient.InjectIP(ip)
			inject.finish(token, 1, len(ip), ctx.Err() == nil)
		}
		packet.release()
		return true
	}
	// Only this session's correlated echoes renew health. Protocol-owned
	// stale echoes are dropped; application gateway pings reach the TUN.
	if s.stats.observeReply(ip, generation, s.id) {
		packet.release()
		return true
	}
	if !diagnosticEnqueue(ctx, s.tunOutbound, packet, enqueue) {
		packet.release()
		return false
	}
	return true
}
