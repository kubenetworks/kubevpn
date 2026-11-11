// Package network owns the local VPN networking runtime: transport recovery,
// address allocation, TUN setup, routes and DNS. It does not own the daemon's
// connection registry, proxy resources or session persistence.
//
// Runtime coordinates startup and cleanup. Its transport supervisor owns
// connection generations, health monitoring and worker admission; watcher
// effects are fenced against retired generations. Address, route, device and
// DNS state remain separate components coordinated by Runtime.
//
// Done reports cancellation, not completed cleanup. The session owner calls
// Stop to close transports and join owned workers before tearing down its
// broader session. Err preserves the terminal transport cause.
package network
