// Package localproxy provides a local SOCKS5/HTTP CONNECT proxy server
// that forwards traffic to Kubernetes services via kubectl port-forward.
//
// Used as a fallback when OS-level TUN routing is not available, e.g.,
// when another VPN client owns the default route (nested VPN case).
// CLI command: kubevpn proxy-out
package localproxy
