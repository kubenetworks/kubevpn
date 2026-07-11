// Package inject implements sidecar injection strategies for Kubernetes workloads.
//
// Three strategies selected by NewInjector factory:
//   - vpnInjector: VPN-only sidecar (no traffic splitting)
//   - meshInjector: VPN + Envoy sidecar (header-based traffic splitting)
//   - fargateInjector: SSH + Envoy sidecar (for environments without NET_ADMIN)
//
// Envoy routing rules are stored as YAML in the traffic manager ConfigMap.
// See docs/05-owner-id.md for the OwnerID ownership model.
package inject
