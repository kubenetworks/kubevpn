//go:build windows && arm
// +build windows,arm

package wintun

import (
	"embed"

	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

//go:embed bin/arm/wintun.dll
var wintunFs embed.FS

func InstallWintunDriver() error {
	bytes, err := wintunFs.ReadFile("bin/arm/wintun.dll")
	if err != nil {
		err = errors.Wrap(err, "wintunFs.ReadFile("bin/arm/wintun.dll"): ")
		return err
	}
	return copyDriver(bytes)
}
