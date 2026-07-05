// Package webhook implements a Kubernetes admission webhook that intercepts
// pod creation events to inject DHCP-allocated IP addresses and network
// configuration required for KubeVPN connectivity.
package webhook
