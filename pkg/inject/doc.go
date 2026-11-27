// Package inject writes node-boundary attachment state for workloads and
// Services without modifying their PodSpecs or Service target ports.
//
// Envoy routing rules are stored as YAML in the traffic manager ConfigMap and
// projected into per-node direct listeners. See docs/05-owner-id.md for the
// OwnerID ownership model and docs/55-node-hosted-agent.md for the data plane.
package inject
