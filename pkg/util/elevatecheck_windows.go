//go:build windows
// +build windows

package util

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/sirupsen/logrus"
	"golang.org/x/sys/windows"

	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

// ref https://stackoverflow.com/questions/31558066/how-to-ask-for-administer-privileges-on-windows-with-go
func RunWithElevated() {
	verb := "runas"
	exe, _ := os.Executable()
	cwd, _ := os.Getwd()
	args := strings.Join(os.Args[1:], " ")

	verbPtr, _ := windows.UTF16PtrFromString(verb)
	exePtr, _ := syscall.UTF16PtrFromString(exe)
	cwdPtr, _ := syscall.UTF16PtrFromString(cwd)
	argPtr, _ := syscall.UTF16PtrFromString(args)

	var showCmd int32 = 1 //SW_NORMAL

	err := windows.ShellExecute(0, verbPtr, exePtr, argPtr, cwdPtr, showCmd)
	if err != nil {
		logrus.Warn(err)
	}
}

// still can't use env KUBECONFIG
func RunWithElevatedInnerExec() error {
	var si windows.StartupInfo
	var pi windows.ProcessInformation
	path, err := exec.LookPath("Powershell")
	if err != nil {
		err = errors.Wrap(err, "exec.LookPath(\"Powershell\"): ")
		return err
	}
	executable, _ := os.Executable()
	join := strings.Join(append([]string{executable}, os.Args[1:]...), " ")
	// Powershell Start C:\Users\naison\Desktop\kubevpn-windows-amd64.exe  -Verb Runas -Wait -WindowStyle Hidden
	c, _ := syscall.UTF16PtrFromString(fmt.Sprintf(`%s Start "%s" -Verb Runas`, path, join))
	err = windows.CreateProcess(nil, c, nil, nil, true, windows.INHERIT_PARENT_AFFINITY, nil, nil, &si, &pi)
	if err != nil {
		err = errors.Wrap(err, "windows.CreateProcess(nil, c, nil, nil, true, windows.INHERIT_PARENT_AFFINITY, nil, nil, &si, &pi): ")
		return err
	}
	p, err := os.FindProcess(int(pi.ProcessId))
	if err != nil {
		err = errors.Wrap(err, "os.FindProcess(int(pi.ProcessId)): ")
		return err
	}
	_, err = p.Wait()
	return err
}

// elevated := windows.GetCurrentProcessToken().IsElevated()
//
//	fmt.Printf("admin %v\n", elevated)
//	return elevated
func IsAdmin() bool {
	_, err := os.Open("\\\\.\\PHYSICALDRIVE0")
	if err != nil {
		return false
	}
	return true
}
