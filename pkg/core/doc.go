// Package core implements the TUN-based network tunnel protocol using gvisor.
// It handles packet routing, TCP/UDP forwarding, connection pooling, and the
// gvisor userspace network stack between the local TUN device and the remote
// traffic manager pod.
//
// Key types:
//   - Server: pairs a net.Listener with a Handler
//   - Forwarder: dials remote with retry and transport wrapping
//   - RouteHub: maps client IPs to their TCP connections for routing
//   - ClientDevice: manages the client-side connection pool (N parallel slots)
//
// This package should NOT contain Kubernetes API calls, DNS config, or sidecar injection.
package core
