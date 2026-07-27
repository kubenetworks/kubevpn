package core

import (
	"sync"
	"sync/atomic"

	"gvisor.dev/gvisor/pkg/tcpip/link/sniffer"
)

// Per-packet data-plane tracing (kubevpn's logIPPacket + gvisor's link-layer sniffer) is a
// hot-path cost: every packet is formatted and written to the daemon log synchronously, which
// caps throughput (a bulk transfer produced ~100MB of per-packet lines per few minutes; see
// docs/50). It is therefore OFF by default and opt-in, driven by the user's --debug intent.
//
// It is NOT gated on the daemon's per-RPC logger level: that logger always records the file at
// Debug by design (a full control-plane record — see log.IsDebugEnabled), so gating per-packet
// logging on it would keep it permanently on. Instead callers Acquire it for the lifetime of a
// --debug data-plane connection and release it on teardown; a reference count keeps it on while
// any such connection is active and turns it off (default) once the last one releases.
var (
	packetLogRefs    atomic.Int32
	packetLogEnabled atomic.Bool
)

func init() {
	// gvisor's sniffer.LogPackets defaults to 1 (every wrapped stack logs every packet). Turn it
	// off at process start so no binary (root daemon, traffic-manager pod, tests) floods logs
	// unless tracing is explicitly acquired.
	sniffer.LogPackets.Store(0)
}

// AcquirePacketLogging turns per-packet data-plane logging on for the caller's lifetime and
// returns a release func. Logging stays on until every acquirer releases (reference counted), so
// concurrent --debug connections coexist and the last release restores the default (off). The
// returned release is idempotent (safe to call more than once).
func AcquirePacketLogging() (release func()) {
	if packetLogRefs.Add(1) == 1 {
		packetLogEnabled.Store(true)
		sniffer.LogPackets.Store(1)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if packetLogRefs.Add(-1) == 0 {
				packetLogEnabled.Store(false)
				sniffer.LogPackets.Store(0)
			}
		})
	}
}

// packetLoggingEnabled reports whether per-packet data-plane logging is currently on. Used by
// logIPPacket to skip the parse+format cost on the hot path when tracing is not acquired.
func packetLoggingEnabled() bool { return packetLogEnabled.Load() }
