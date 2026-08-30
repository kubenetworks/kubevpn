// Package controlplane holds the envoy xDS configuration data types that are
// stored (as YAML) in the traffic-manager ConfigMap and shared between the
// sidecar-injection logic (pkg/inject) and the xDS snapshot builder (pkg/xds).
//
// These types live in a leaf package — depending only on the Kubernetes core
// types and the standard library — so both pkg/inject and pkg/xds can import
// them without creating an import cycle (pkg/inject no longer imports pkg/xds,
// which lets pkg/xds import pkg/inject to run injection server-side in the pod).
//
// The envoy-resource *building* logic (turning a Virtual into listeners /
// clusters / routes / endpoints) stays in pkg/xds, since it pulls in the heavy
// go-control-plane dependency tree.
package controlplane

import (
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// CurrentSchemaVersion is the latest schema version for Virtual configs stored in ConfigMaps.
// Bump this when making breaking changes to the Virtual/Rule struct layout.
// A zero value means a legacy config created before versioning was introduced.
// Version 2: OwnerID is required on all rules (no backward compat with empty OwnerID).
const CurrentSchemaVersion = 2

// Virtual represents an envoy xDS configuration for a single proxied workload.
type Virtual struct {
	// SchemaVersion tracks the config schema revision. Zero means legacy (pre-versioning).
	SchemaVersion int `yaml:"schemaVersion,omitempty" json:"schemaVersion,omitempty"`
	Namespace     string
	UID           string `yaml:"Uid" json:"Uid"` // group.resource.name
	FargateMode   bool   `yaml:"fargateMode,omitempty" json:"fargateMode,omitempty"`
	Ports         []ContainerPort
	Rules         []*Rule
}

// ContainerPort describes a port on a container with optional envoy listener binding.
type ContainerPort struct {
	// If specified, this must be an IANA_SVC_NAME and unique within the pod. Each
	// named port in a pod must have a unique name. Name for the port that can be
	// referred to by services.
	// +optional
	Name string `json:"name,omitempty"`
	// EnvoyListenerPort is the port envoy binds to in fargate mode (BindToPort=true).
	// In mesh mode this is 0 (envoy uses iptables-redirected traffic instead).
	// +optional
	EnvoyListenerPort int32 `json:"envoyListenerPort,omitempty"`
	// Number of port to expose on the pod's IP address.
	// This must be a valid port number, 0 < x < 65536.
	ContainerPort int32 `json:"containerPort"`
	// Protocol for port. Must be UDP, TCP, or SCTP.
	// Defaults to "TCP".
	// +optional
	// +default="TCP"
	Protocol corev1.Protocol `json:"protocol,omitempty"`
}

// IsFargateMode returns true if this Virtual uses Fargate/Service mode
// (no iptables, SSH tunnels instead of VPN sidecar).
// Checks the explicit FargateMode field first, with a fallback to the legacy
// heuristic (EnvoyListenerPort != 0) for backward compatibility with existing ConfigMaps.
func (a *Virtual) IsFargateMode() bool {
	if a.FargateMode {
		return true
	}
	for _, port := range a.Ports {
		if port.EnvoyListenerPort != 0 {
			return true
		}
	}
	return false
}

// ConvertContainerPort converts Kubernetes ContainerPort values to controlplane ContainerPort with EnvoyListenerPort=0.
func ConvertContainerPort(ports ...corev1.ContainerPort) []ContainerPort {
	var result []ContainerPort
	for _, port := range ports {
		result = append(result, ContainerPort{
			Name:              port.Name,
			EnvoyListenerPort: 0,
			ContainerPort:     port.ContainerPort,
			Protocol:          port.Protocol,
		})
	}
	return result
}

// Rule defines a header-based routing rule for envoy traffic splitting.
type Rule struct {
	Headers      map[string]string
	LocalTunIPv4 string
	LocalTunIPv6 string
	// OwnerID identifies the connection that owns this rule (a UUID prefix).
	// Required — used as the primary key for rule matching in addVirtualRule and removeEnvoyConfig.
	OwnerID string `yaml:"ownerID" json:"ownerID"`
	// for no privileged mode (AWS Fargate mode), don't have cap NET_ADMIN and privileged: true. so we cannot use OSI layer 3 proxy
	// containerPort -> envoyRulePort:localPort
	// envoyRulePort for envoy forward to localhost:envoyRulePort
	// localPort for local pc listen localhost:localPort
	// use ssh reverse tunnel, envoy rule endpoint localhost:envoyRulePort will forward to local pc localhost:localPort
	// localPort is required and envoyRulePort is optional
	PortMap map[int32]string
}

// PortMapping represents a parsed port mapping from the PortMap string encoding.
type PortMapping struct {
	// ContainerPort is the original container port (the map key in PortMap).
	ContainerPort int32
	// EnvoyPort is the port envoy forwards to (the first number in the string value).
	EnvoyPort int32
	// LocalPort is the port the local PC listens on.
	// In non-fargate mode (plain "envoyPort" format), LocalPort equals ContainerPort.
	// In fargate mode ("envoyPort:localPort" format), LocalPort is the second number.
	LocalPort int32
}

// ParsePortMap parses the string-encoded PortMap into typed PortMapping values.
// The string value is either "envoyPort" (plain number) or "envoyPort:localPort" (colon-separated pair).
func (r *Rule) ParsePortMap() []PortMapping {
	result := make([]PortMapping, 0, len(r.PortMap))
	for containerPort, portStr := range r.PortMap {
		pm := PortMapping{ContainerPort: containerPort}
		if before, after, ok := strings.Cut(portStr, ":"); ok {
			pm.EnvoyPort, _ = parsePort(before)
			pm.LocalPort, _ = parsePort(after)
		} else {
			pm.EnvoyPort, _ = parsePort(portStr)
			pm.LocalPort = containerPort
		}
		result = append(result, pm)
	}
	return result
}

// parsePort converts a port string to int32, returning 0 on parse failure.
func parsePort(s string) (int32, bool) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return int32(n), true
}
