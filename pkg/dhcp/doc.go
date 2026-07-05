// Package dhcp manages TUN device IP allocation via a Kubernetes ConfigMap.
//
// Uses a bitmap-based allocator (cilium/ipam) with optimistic locking
// (RetryOnConflict) for concurrent multi-user IP assignment from the
// 198.18.0.0/16 (IPv4) and 2001:2::/64 (IPv6) pools.
package dhcp
