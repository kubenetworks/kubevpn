// Package inject implements sidecar injection strategies for Kubernetes workloads.
//
// Two strategies selected by NewInjector factory:
//   - meshInjector: VPN + Envoy sidecar. With --headers it does header-based traffic
//     splitting; with empty headers envoy matches all requests, giving full traffic
//     interception (the unified replacement for the former VPN-only injector).
//   - fargateInjector: SSH + Envoy sidecar (for environments without NET_ADMIN)
//
// Envoy routing rules are stored as YAML in the traffic manager ConfigMap.
// See docs/05-owner-id.md for the OwnerID ownership model.
package inject
