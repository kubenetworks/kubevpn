package run

import (
	"strings"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
)

func randomSuffix() string {
	return strings.ReplaceAll(uuid.New().String(), "-", "")[:5]
}

// containerSecurityOpts returns the common Docker security flags
// shared by both VPN connect containers and dev workload containers.
func containerSecurityOpts() []string {
	return []string{
		"--cap-add", "SYS_PTRACE",
		"--cap-add", "SYS_ADMIN",
		"--security-opt", "apparmor=unconfined",
		"--security-opt", "seccomp=unconfined",
	}
}

// Pull constants
const (
	PullImageAlways  = "always"
	PullImageMissing = "missing" // Default (matches previous behavior)
	PullImageNever   = "never"
)

func ConvertK8sImagePullPolicyToDocker(policy corev1.PullPolicy) string {
	switch policy {
	case corev1.PullAlways:
		return PullImageAlways
	case corev1.PullNever:
		return PullImageNever
	default:
		return PullImageMissing
	}
}

func convertToDockerArgs(runConfig *RunConfig) []string {
	result := append([]string{"docker", "run"}, runConfig.options...)
	if len(runConfig.command) != 0 {
		result = append(result, "--entrypoint", strings.Join(runConfig.command, " "))
	}
	result = append(result, runConfig.image)
	result = append(result, runConfig.args...)
	return result
}
