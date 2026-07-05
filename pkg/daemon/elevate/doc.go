// Package elevate provides platform-specific privilege escalation for the
// KubeVPN daemon. It detects whether the current process has sufficient
// permissions and re-launches with elevated privileges when necessary.
package elevate
