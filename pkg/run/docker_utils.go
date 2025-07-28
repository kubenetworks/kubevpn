package run

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
)

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
	var result = []string{"docker"}
	result = append(result, "run")
	result = append(result, runConfig.options...)
	if len(runConfig.command) != 0 {
		result = append(result, "--entrypoint", strings.Join(runConfig.command, " "))
	}
	result = append(result, runConfig.image)
	result = append(result, runConfig.args...)
	return result
}
