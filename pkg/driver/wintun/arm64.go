//go:build windows && arm64
// +build windows,arm64

package wintun

import (
	"embed"
)

//go:embed bin/arm64/wintun.dll
var wintunFs embed.FS

func InstallWintunDriver() error {
	bytes, err := wintunFs.ReadFile("bin/arm64/wintun.dll")
	if err != nil {
		err = errors.Wrap(err, "wintunFs.ReadFile("bin/arm64/wintun.dll"): ")
		return err
	}
	return copyDriver(bytes)
}
