//go:build !windows
// +build !windows

package wintun

import (
	"github.com/pkg/errors"
)

func InstallWintunDriver() error {
	return errors.New("not implement")
}
