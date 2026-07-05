// Package netutil provides pure network and transport primitives for KubeVPN:
// raw IP packet parsing, ICMP generation, TUN device lookup, TLS helpers,
// gvisor proxy-info serialization, and panic recovery.
// It has no Kubernetes cluster-API or Docker dependencies.
package netutil
