//go:build !windows

package wintun

import (
	"errors"
)

func InstallWintunDriver() error {
	return errors.New("not implement")
}
