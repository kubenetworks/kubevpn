//go:build windows && x86
// +build windows,x86

package wintun

import (
	"embed"
	"io/ioutil"
	"os"
	"path/filepath"
)

//go:embed bin/x86/wintun.dll
var wintunFs embed.FS

func InstallWintunDriver() error {
	bytes, err := wintunFs.ReadFile("wintun.dll")
	if err != nil {
		return err
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	filename := filepath.Join(wd, "wintun.dll")
	_ = os.Remove(filename)
	err = ioutil.WriteFile(filename, bytes, 644)
	return err
}
