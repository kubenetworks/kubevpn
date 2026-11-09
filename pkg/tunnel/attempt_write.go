package tunnel

import (
	"context"
	"fmt"
	"net"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

// writeGeneration drains this attempt's inbound queue, writes batches with datagram
// framing, and records probe and activity evidence.
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
	// Reverse traffic is not flow-affine to this slot: the server may send it on any registered
	// conn. A slot busy sending can therefore be healthy while receiving nothing. Keep its read
	// deadline alive on write activity so a bulk transfer is not torn down after KeepAliveTime*3.
	// framing.Conn embeds this same conn, so both goroutines safely push the underlying deadline.
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
		if s.isControl && packet.packetType() == packetTypeControl {
			ip, id := s.stats.prepareProbe(packet.ip(), generation, s.id)
			if ip == nil {
				batch.release()
				continue
			}
			probeID = id
			packet.setIP(ip)
		}
		for _, p := range batch.packets[:batch.n] {
			p.stampLength()
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
