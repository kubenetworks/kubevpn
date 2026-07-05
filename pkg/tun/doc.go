// Package tun creates and manages TUN network devices and their routes.
//
// Platform-specific implementations:
//   - Linux: /dev/net/tun + netlink
//   - macOS: utun + ioctl
//   - Windows: WireGuard wintun driver
package tun
