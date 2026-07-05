//go:build windows

package dns

// withHostsFileLock is a no-op on Windows because syscall.Flock is not
// available. The function simply executes fn directly.
func withHostsFileLock(fn func() error) error {
	return fn()
}
