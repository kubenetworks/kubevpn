package config

import "errors"

var (
	// ErrInvalidKubeconfig indicates the provided kubeconfig file is missing or malformed.
	ErrInvalidKubeconfig = errors.New("kubeconfig is invalid")
	// ErrPortForwardTimeout indicates a kubectl port-forward did not become ready in time.
	ErrPortForwardTimeout = errors.New("port-forward readiness timeout")
	// ErrPermissionDenied indicates a privilege/credential failure (not root, RBAC forbidden).
	// Wrap cluster RBAC/authorization errors with %w to surface a permission exit code.
	ErrPermissionDenied = errors.New("permission denied")
	// ErrNotFound indicates a required cluster resource is missing.
	// Wrap Kubernetes NotFound errors with %w to surface a not-found exit code.
	ErrNotFound = errors.New("not found")
)
