//go:build windows

package elevate

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// ref https://stackoverflow.com/questions/31558066/how-to-ask-for-administer-privileges-on-windows-with-go
func RunCmdWithElevated(exe string, arg []string) error {
	verb := "runas"
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

	//https://learn.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-showwindow
	var showCmd int32 = 0 //SW_NORMAL

	os.Setenv(config.EnvDisableSyncthingLog, "1")
	err = windows.ShellExecute(0, verbPtr, exePtr, argPtr, cwdPtr, showCmd)
	if err != nil {
		plog.G(context.Background()).Warn(err)
	}
	return err
}

func RunCmd(exe string, arg []string) error {
	verb := "open"
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

	//https://learn.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-showwindow
	var showCmd int32 = 0 //SW_NORMAL

	err = windows.ShellExecute(0, verbPtr, exePtr, argPtr, cwdPtr, showCmd)
	if err != nil {
		plog.G(context.Background()).Warn(err)
	}
	return err
}

func Kill(cmd *exec.Cmd) error {
	kill := exec.Command("TASKKILL", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	kill.Stderr = os.Stderr
	kill.Stdout = os.Stdout
	return kill.Run()
}
