package config

import "errors"

var (
	// ErrInvalidKubeconfig indicates the provided kubeconfig file is missing or malformed.
	ErrInvalidKubeconfig = errors.New("kubeconfig is invalid")
	// ErrPortForwardTimeout indicates a kubectl port-forward did not become ready in time.
	ErrPortForwardTimeout = errors.New("port-forward readiness timeout")
)
