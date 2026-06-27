// Package dns configures system DNS to resolve Kubernetes service names.
//
// Platform-specific implementations:
//   - Linux: systemd-resolved, library DNS configurator, or /etc/resolv.conf
//   - macOS: /etc/resolver/ directory files
//   - Windows: LUID interface DNS APIs
//
// Host file operations (/etc/hosts) use flock for cross-process safety.
package dns
