// Package network owns the local VPN networking runtime: transport recovery,
// address allocation, TUN setup, routes and DNS. It does not own the daemon's
// connection registry, proxy resources or session persistence.
//
// Runtime coordinates startup and cleanup. Its serial transport supervisor owns
// prepare/publish/retire/retry decisions and worker admission. Each
// transportGeneration owns endpoint handles, returned connections and pending
// dials; generationHealth only evaluates heartbeat evidence, and the concrete
// retry policy only computes the next recovery decision. Watcher effects are
// fenced against retired generations. Address, route, device and DNS state
// remain separate components coordinated by Runtime.
//
// Done reports cancellation, not completed cleanup. The session owner calls
// Stop to close transports and join owned workers before tearing down its
// broader session. Err preserves the terminal transport cause.
package network
