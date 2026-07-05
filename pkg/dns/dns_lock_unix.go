//go:build !windows

package dns

import (
	"os"
	"syscall"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// withHostsFileLock acquires an exclusive flock on a lock file adjacent to the
// hosts file, executes fn, and releases the lock. If the lock file cannot be
// created or locked, fn is executed without locking as a fallback so that
// single-instance usage is not broken.
func withHostsFileLock(fn func() error) error {
	lockFile, err := os.OpenFile(getHostFile()+".lock", os.O_CREATE|os.O_RDONLY, config.FileModeFile)
	if err != nil {
		return fn()
	}
	defer lockFile.Close()

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fn()
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck

	return fn()
}
