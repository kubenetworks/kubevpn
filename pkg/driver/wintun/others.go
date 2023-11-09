//go:build !windows
// +build !windows

package wintun

import (
	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

func InstallWintunDriver() error {
	return errors.New("not implement")
}
