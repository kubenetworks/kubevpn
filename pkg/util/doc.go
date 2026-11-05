// Package util provides shared client initialization, kubeconfig conversion,
// file, process, port and formatting helpers used by KubeVPN's command adapters.
// Existing command-facing entry points remain here; Kubernetes port forwarding,
// workload operations and cluster discovery belong to package cluster.
//
// Helpers with their own state and lifecycle belong to the domain that owns
// those resources, not this package. Independent shared mechanisms live in the
// netutil, exitcode, progress, regctl and krew subpackages. A helper does not need
// a new package solely because its file is large or it has several callers.
package util
