//go:build windows
// +build windows

package util

import (
	"os"
	"strings"
	"syscall"

	"github.com/sirupsen/logrus"
	"golang.org/x/sys/windows"
)

// ref https://stackoverflow.com/questions/31558066/how-to-ask-for-administer-privileges-on-windows-with-go
func RunCmdWithElevated(arg []string) error {
	verb := "runas"
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	args := strings.Join(arg, " ")

	verbPtr, err := windows.UTF16PtrFromString(verb)
	if err != nil {
		return err
	}
	exePtr, err := syscall.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	cwdPtr, err := syscall.UTF16PtrFromString(cwd)
	if err != nil {
		return err
	}
	argPtr, err := syscall.UTF16PtrFromString(args)
	if err != nil {
		return err
	}

	var showCmd int32 = 1 //SW_NORMAL

	err = windows.ShellExecute(0, verbPtr, exePtr, argPtr, cwdPtr, showCmd)
	if err != nil {
		logrus.Warn(err)
	}
	return err
}
