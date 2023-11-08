//go:build windows && (x86 || 386)
// +build windows
// +build x86 386

package wintun

import (
	"embed"
	"github.com/wencaiwulue/kubevpn/pkg/errors"

)

//go:embed bin/x86/wintun.dll
var wintunFs embed.FS

func InstallWintunDriver() error {
	bytes, err := wintunFs.ReadFile("bin/x86/wintun.dll")
	if err != nil {
		err = errors.Wrap(err, "Failed to read wintun.dll file for x86 ")
		return err
	}
	return copyDriver(bytes)
}
