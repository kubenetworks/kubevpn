// Package exitcode maps daemon/command errors to a small, stable set of process
// exit codes so that scripts invoking the kubevpn CLI can distinguish failure
// categories. Classification is carried over gRPC status codes (set by the
// daemon's classify interceptor) rather than a bespoke protocol field, so a
// failed streaming command — which breaks the stream with a non-OK status — maps
// cleanly to an exit code.
package exitcode

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Process exit codes returned by the kubevpn CLI.
const (
	// Success is returned when a command completes without error.
	Success = 0
	// Generic is any unclassified failure.
	Generic = 1
	// BadConfig is an invalid argument, malformed config, or unmet precondition.
	BadConfig = 2
	// Permission is a privilege/credential failure (not root, RBAC forbidden, bad credentials).
	Permission = 3
	// ClusterNetwork is a transient cluster/network reachability failure or timeout.
	ClusterNetwork = 4
	// NotFound is a missing cluster resource (workload, connection, namespace, ...).
	NotFound = 5
	// Interrupted is returned when the command is aborted by SIGINT (Unix 128+SIGINT).
	Interrupted = 130
)

// FromError maps an error returned from command execution to a process exit code.
// A nil error yields Success. Errors carrying a gRPC status (the daemon attaches
// one via its classify interceptor) are mapped by their code; any other error is
// Generic.
func FromError(err error) int {
	if err == nil {
		return Success
	}
	st, ok := status.FromError(err)
	if !ok {
		return Generic
	}
	switch st.Code() {
	case codes.OK:
		return Success
	case codes.Canceled:
		return Interrupted
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		return BadConfig
	case codes.PermissionDenied, codes.Unauthenticated:
		return Permission
	case codes.Unavailable, codes.DeadlineExceeded:
		return ClusterNetwork
	case codes.NotFound:
		return NotFound
	default:
		return Generic
	}
}
