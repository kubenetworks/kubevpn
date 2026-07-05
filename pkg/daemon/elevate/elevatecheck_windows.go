//go:build windows

package elevate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// RunWithElevated re-executes the current process with elevated (sudo/admin) privileges.
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

	os.Setenv(EnvDisableSyncthingLog, "1")
	err := windows.ShellExecute(0, verbPtr, exePtr, argPtr, cwdPtr, showCmd)
	if err != nil {
		plog.G(context.Background()).Warn(err)
	}
}

// still can't use env KUBECONFIG
func RunWithElevatedInnerExec() error {
	var si windows.StartupInfo
	var pi windows.ProcessInformation
	path, err := exec.LookPath("Powershell")
	if err != nil {
		return err
	}
	executable, _ := os.Executable()
	join := strings.Join(append([]string{executable}, os.Args[1:]...), " ")
	// Powershell Start C:\Users\naison\Desktop\kubevpn-windows-amd64.exe  -Verb Runas -Wait -WindowStyle Hidden
	c, _ := syscall.UTF16PtrFromString(fmt.Sprintf(`%s Start "%s" -Verb Runas`, path, join))
	env, _ := syscall.UTF16PtrFromString(EnvDisableSyncthingLog + "=1")
	err = windows.CreateProcess(nil, c, nil, nil, true, windows.INHERIT_PARENT_AFFINITY, env, nil, &si, &pi)
	if err != nil {
		return err
	}
	p, err := os.FindProcess(int(pi.ProcessId))
	if err != nil {
		return err
	}
	_, err = p.Wait()
	return err
}

// IsAdmin reports whether the current process is running with root/administrator privileges.
func IsAdmin() bool {
	f, err := os.Open("\\\\.\\PHYSICALDRIVE0")
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}
