//go:build !windows

package config

import "os"

// migratePrivatePath tightens permissions only as the unprivileged user. The
// root daemon shares these paths but must not chmod a user-controlled path.
func migratePrivatePath(path string, mode os.FileMode) error {
	if os.Geteuid() == 0 {
		return nil
	}
	return os.Chmod(path, mode)
}
