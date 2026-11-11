//go:build windows

package action

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// Windows does not support syncing a directory handle. Rename still replaces the
// directory entry, while the file itself has already been synced and closed.
func syncDirectory(string) error { return nil }

// isTransientDBReadError reports whether a failed read of the persisted DB should be
// retried. A reader that opens the DB while the atomic write's rename is in flight can
// hit a sharing or lock violation (the destination is briefly delete-pending); both are
// transient and resolve once the rename completes.
func isTransientDBReadError(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == windows.ERROR_SHARING_VIOLATION || errno == windows.ERROR_LOCK_VIOLATION
}
