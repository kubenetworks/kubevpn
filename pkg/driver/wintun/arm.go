//go:build windows && arm

package wintun

import (
	"embed"
)

//go:embed bin/arm/wintun.dll
var wintunFs embed.FS

func InstallWintunDriver() error {
	bytes, err := wintunFs.ReadFile("bin/arm/wintun.dll")
	if err != nil {
		return err
	}
	return copyDriver(bytes)
}
