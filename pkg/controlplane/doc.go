// Package controlplane implements the Envoy xDS control plane.
//
// It watches the traffic manager ConfigMap for Virtual/Rule configuration
// changes and generates envoy Listener, Cluster, Route, and Endpoint
// snapshots via the go-control-plane snapshot cache.
package controlplane
