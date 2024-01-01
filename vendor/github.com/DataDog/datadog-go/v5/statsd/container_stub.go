//go:build !linux
// +build !linux

package statsd

func initContainerID(userProvidedID string, cgroupFallback bool) {
	initOnce.Do(func() {
		if userProvidedID != "" {
			containerID = userProvidedID
			return
		}
	})
}
