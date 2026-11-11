//go:build !windows

package action

import (
	"errors"
	"os"
)

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// isTransientDBReadError reports whether a failed read of the persisted DB should be
// retried. On unix the atomic write replaces the directory entry, so a concurrent
// reader either sees the old or the new file; only a missing file is transient.
func isTransientDBReadError(err error) bool { return errors.Is(err, os.ErrNotExist) }
