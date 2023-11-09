//go:build windows && amd64
// +build windows,amd64

package wintun

import (
	"embed"

	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

//go:embed bin/amd64/wintun.dll
var wintunFs embed.FS

func InstallWintunDriver() error {
	bytes, err := wintunFs.ReadFile("bin/amd64/wintun.dll")
	if err != nil {
		err = errors.Wrap(err, "Failed to read wintun.dll file for amd64 ")
		return err
	}
	return copyDriver(bytes)
}
