//go:build !linux
// +build !linux

package statsd

var initContainerID = func(userProvidedID string, cgroupFallback bool) {
	initOnce.Do(func() {
		if userProvidedID != "" {
			containerID = userProvidedID
			return
		}
	})
}
