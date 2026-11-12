//go:build windows

package action

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// Windows does not support syncing a directory handle. Rename still replaces the
// directory entry, while the file itself has already been synced and closed.
func syncDirectory(string) error { return nil }

// isTransientDBReadError reports whether a failed read of the persisted DB should be
// retried. The file is missing before the first write; a reader that opens it while the
// atomic write's rename is in flight sees either a sharing/lock violation (destination
// briefly delete-pending) or a transient not-found once the entry is replaced. All are
// transient and resolve once the rename completes.
func isTransientDBReadError(err error) bool {
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	return isTransientReplaceError(err)
}

// isTransientReplaceError reports whether a failed rename over an existing file should
// be retried. On Windows a concurrent reader holding the previous file keeps it
// delete-pending, briefly denying both its replacement (access denied) and new opens
// (sharing/lock violation); every state clears once the last reader handle closes.
func isTransientReplaceError(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == windows.ERROR_ACCESS_DENIED ||
		errno == windows.ERROR_SHARING_VIOLATION ||
		errno == windows.ERROR_LOCK_VIOLATION
}
