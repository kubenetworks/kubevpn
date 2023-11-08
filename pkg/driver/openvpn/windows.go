//go:build windows
// +build windows

package openvpn

import (
	"embed"
	"io/ioutil"
	"os"
	"os/exec"

	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

//go:embed exe/tap-windows-9.21.2.exe
var fs embed.FS

// driver download from https://build.openvpn.net/downloads/releases/
func Install() error {
	bytes, err := fs.ReadFile("exe/tap-windows-9.21.2.exe")
	if err != nil {
		err = errors.Wrap(err, "Failed to read tap-windows exe file ")
		return err
	}
	tempFile, err := ioutil.TempFile("", "*.exe")
	defer func() { _ = os.Remove(tempFile.Name()) }()
	if err != nil {
		err = errors.Wrap(err, "Failed to remove temporary file ")
		return err
	}
	if _, err = tempFile.Write(bytes); err != nil {
		return err
	}
	_ = tempFile.Sync()
	_ = tempFile.Close()
	_ = os.Chmod(tempFile.Name(), 0700)
	cmd := exec.Command(tempFile.Name(), "/S")
	return cmd.Run()
}
