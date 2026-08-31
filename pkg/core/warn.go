package core

import (
	"time"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// dataPlaneWarnInterval paces the data plane's recurring failure warnings. The events these guard
// repeat on a timer (heartbeat every config.HeartbeatInterval, slot reconnect every few seconds), so
// they need rate limiting; but they must not be Debug-level either — a client silently announcing no
// route and a server silently dropping every echo reply for want of one took a tunnel down for 20
// hours while the log stayed clean. One line per key per interval is the compromise.
const dataPlaneWarnInterval = 30 * time.Second

// dataPlaneWarn rate-limits the data plane's recurring failure warnings. Keys identify what recurs:
// a call site for client-side failures, a peer address for per-destination drops.
var dataPlaneWarn = plog.NewThrottle(dataPlaneWarnInterval)
