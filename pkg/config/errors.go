package config

import "errors"

// Sentinel errors for common failure conditions.
// Use errors.Is() to check these from calling code.
var (
	ErrDHCPExhausted      = errors.New("DHCP address pool exhausted")
	ErrConnectionTimeout  = errors.New("connection establishment timeout")
	ErrPortInUse          = errors.New("port already in use")
	ErrClusterUnreachable = errors.New("cluster API server unreachable")
	ErrSidecarNotFound    = errors.New("sidecar container not found in pod")
	ErrInvalidKubeconfig  = errors.New("kubeconfig is invalid")
	ErrPortForwardTimeout = errors.New("port-forward readiness timeout")
	ErrTUNDeviceCreate    = errors.New("failed to create TUN device")
)
