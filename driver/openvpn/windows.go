//go:build windows
// +build windows

package openvpn

import (
	"embed"
	"io/ioutil"
	"os"
	"os/exec"
)

//go:embed exe/tap-windows-9.21.2.exe
var fs embed.FS

func Install() error {
	bytes, err := fs.ReadFile("tap-windows-9.21.2.exe")
	if err != nil {
		return err
	}
	tempFile, err := ioutil.TempFile("", "*.exe")
	defer func() { _ = os.Remove(tempFile.Name()) }()
	if err != nil {
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
