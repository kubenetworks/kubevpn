//go:build windows

package config

import "os"

// POSIX permission bits are not a Windows ACL boundary.
func migratePrivatePath(string, os.FileMode) error { return nil }
