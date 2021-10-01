//go:build !windows
// +build !windows

package wintun

func InstallWintunDriver() error {
	return errors.New("not implement")
}
