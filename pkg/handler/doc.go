// Package handler contains the core business logic for kubevpn operations.
//
// ConnectOptions is the central struct — it orchestrates VPN connection setup,
// proxy sidecar injection, file synchronization, and resource cleanup. It exists
// as TWO independent instances in the dual-daemon architecture:
//   - User Daemon (control plane): DHCP, proxy inject, health check, OwnerID
//   - Root Daemon (data plane): TUN device, routing, DNS, port-forward
//
// See docs/dual-daemon-architecture.md for details.
//
// This package is imported by daemon/action/. It should NOT import daemon/.
package handler
