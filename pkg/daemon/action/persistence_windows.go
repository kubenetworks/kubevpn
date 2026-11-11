//go:build windows

package action

// Windows does not support syncing a directory handle. Rename still replaces the
// directory entry, while the file itself has already been synced and closed.
func syncDirectory(string) error { return nil }
