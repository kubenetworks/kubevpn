package core

import (
	"sync/atomic"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

// isHeartbeatEchoReply reports whether ipPacket is an ICMP echo reply from the server
// gateway (RouterIP / RouterIP6) — i.e. a reply to one of our keepalive heartbeats.
func isHeartbeatEchoReply(ipPacket []byte) bool {
	return netutil.IsICMPEchoReplyFrom(ipPacket, config.RouterIP) ||
		netutil.IsICMPEchoReplyFrom(ipPacket, config.RouterIP6)
}

// HeartbeatStats records the last time an ICMP echo reply from the server gateway
// (config.RouterIP / RouterIP6) was observed on the inbound path. It is the data-plane
// liveness signal: a recent reply means traffic is flowing through the TUN end to end,
// all the way to the server and back. A nil *HeartbeatStats is a no-op (server role).
type HeartbeatStats struct {
	lastReplyUnixNano atomic.Int64
}

// MarkReply records that an echo reply was just observed.
func (h *HeartbeatStats) MarkReply() {
	if h == nil {
		return
	}
	h.lastReplyUnixNano.Store(time.Now().UnixNano())
}

// LastReply returns the time of the last observed echo reply, or the zero time if none.
func (h *HeartbeatStats) LastReply() time.Time {
	if h == nil {
		return time.Time{}
	}
	v := h.lastReplyUnixNano.Load()
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(0, v)
}
