//go:build windows

package openvpn

import (
	"embed"
	"os"
	"os/exec"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

//go:embed exe/tap-windows-9.21.2.exe
var fs embed.FS

// driver download from https://build.openvpn.net/downloads/releases/
func Install() error {
	bytes, err := fs.ReadFile("exe/tap-windows-9.21.2.exe")
	if err != nil {
		return err
	}
	tempFile, err := os.CreateTemp("", "*.exe")
	if err != nil {
		return err
	}
	defer func() { _ = tempFile.Close() }()
	defer func() { _ = os.Remove(tempFile.Name()) }()
	if _, err = tempFile.Write(bytes); err != nil {
		return err
	}
	_ = tempFile.Sync()
	_ = tempFile.Close()
	_ = os.Chmod(tempFile.Name(), config.FileModePrivate)
	cmd := exec.Command(tempFile.Name(), "/S")
	return cmd.Run()
}
