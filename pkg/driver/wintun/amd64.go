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
		err = errors.Wrap(err, "wintunFs.ReadFile(\"bin/amd64/wintun.dll\"): ")
		return err
	}
	return copyDriver(bytes)
}
