package config

import "errors"

var (
	ErrInvalidKubeconfig  = errors.New("kubeconfig is invalid")
	ErrPortForwardTimeout = errors.New("port-forward readiness timeout")
)
