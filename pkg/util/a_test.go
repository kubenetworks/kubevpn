package util

import (
	"context"
	"fmt"
	"github.com/wencaiwulue/kubevpn/pkg/util/wintoken"
	"golang.org/x/sys/windows"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"unsafe"
)

func TestName112399(t *testing.T) {
	verb := "runas"
	//exe, _ := os.Executable()
	exe := "C:\\Users\\naison\\Desktop\\kubevpn-windows-amd64.exe"
	cwd, _ := os.Getwd()
	args := /* strings.Join(os.Args[:], " ")*/ ""

	verbPtr, _ := windows.UTF16PtrFromString(verb)
	exePtr, _ := syscall.UTF16PtrFromString(exe)
	cwdPtr, _ := syscall.UTF16PtrFromString(cwd)
	argPtr, _ := syscall.UTF16PtrFromString(args)

	//https://learn.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-showwindow
	var showCmd int32 = 1 //SW_NORMAL

	//

	var modshell32 = windows.NewLazySystemDLL("shell32.dll")
	var procShellExecuteW = modshell32.NewProc("ShellExecuteW")
	r1, r2, e1 := syscall.Syscall6(procShellExecuteW.Addr(), 6, uintptr(0), uintptr(unsafe.Pointer(verbPtr)), uintptr(unsafe.Pointer(exePtr)), uintptr(unsafe.Pointer(argPtr)), uintptr(unsafe.Pointer(cwdPtr)), uintptr(showCmd))
	if r1 <= 32 {
		panic(e1)
	}
	println(r1)
	println(r2)

	token, err := wintoken.OpenProcessToken(0, wintoken.TokenPrimary)
	if err == nil {
		println(token.Token().IsElevated())
	} else {
		println(err.Error())
	}
	token, err = wintoken.OpenProcessToken(0, wintoken.TokenPrimary)
	if err == nil {
		println(token.Token().IsElevated())
	} else {
		println(err.Error())
	}
	//err := windows.ShellExecute(0, verbPtr, exePtr, argPtr, cwdPtr, showCmd)
	//if e1 != nil {
	//	logrus.Warn(e1)
	//}
}

func TestName112399Exec(t *testing.T) {
	verb := "runas"
	//exe, _ := os.Executable()
	//cwd, _ := os.Getwd()
	//args := /* strings.Join(os.Args[:], " ")*/ ""
	//
	//verbPtr, _ := windows.UTF16PtrFromString(verb)
	//exePtr, _ := syscall.UTF16PtrFromString(exe)
	//cwdPtr, _ := syscall.UTF16PtrFromString(cwd)
	//argPtr, _ := syscall.UTF16PtrFromString(args)

	strings := []string{"/env", "/user:administrator", fmt.Sprintf(`"%s version"`, "C:\\Users\\naison\\Desktop\\kubevpn-windows-amd64.exe")}
	cmd := exec.Command(verb, strings...)
	cancel, cancelFunc := context.WithCancel(context.Background())
	err := cmd.Start()
	if err != nil {
		panic(err)
	}
	go func() {
		_ = cmd.Wait()
		cancelFunc()
	}()
	var to *windows.Token
	for {
		select {
		case <-cancel.Done():
			if to != nil {
				print(to.IsElevated())
				return
			} else {
				panic("not found")
			}
		default:
			token, err := wintoken.OpenProcessToken(cmd.Process.Pid, wintoken.TokenPrimary)
			if err == nil {
				ttt := token.Token()
				to = &ttt
			}
		}
	}
}

func TestName112399CreateProcess(t *testing.T) {
	//verb := "runas"
	//exe, _ := os.Executable()
	cwd, _ := os.Getwd()
	//args := /* strings.Join(os.Args[:], " ")*/ ""
	//
	//verbPtr, _ := windows.UTF16PtrFromString(verb)
	//exePtr, _ := syscall.UTF16PtrFromString(exe)
	cwdPtr, _ := syscall.UTF16PtrFromString(cwd)
	//argPtr, _ := syscall.UTF16PtrFromString(args)

	windows.Setenv("KUBECONFIG", os.Getenv("KUBECONFIG"))
	v, _ := syscall.UTF16PtrFromString("KUBECONFIG=" + os.Getenv("KUBECONFIG"))
	// func CreateProcess(appName *uint16, commandLine *uint16, procSecurity *SecurityAttributes, threadSecurity *SecurityAttributes, inheritHandles bool, creationFlags uint32, env *uint16, currentDir *uint16, startupInfo *StartupInfo, outProcInfo *ProcessInformation) (err error) {
	var si windows.StartupInfo
	var pi windows.ProcessInformation
	a, _ := syscall.UTF16PtrFromString("runas")
	//c, _ := syscall.UTF16PtrFromString("C:\\Users\\naison\\Desktop\\kubevpn-windows-amd64.exe")

	//strings := []string{"/env", "/user:administrator", fmt.Sprintf(`"%s version"`, "C:\\Users\\naison\\Desktop\\kubevpn-windows-amd64.exe")}
	cancel, cancelFunc := context.WithCancel(context.Background())
	go func() {
		err2 := windows.CreateProcess(nil, a, nil, nil, true, windows.NORMAL_PRIORITY_CLASS, v, cwdPtr, &si, &pi)
		if err2 != nil {
			panic(err2)
		}
		cancelFunc()
	}()
	var to *windows.Token
	for {
		select {
		case <-cancel.Done():
			if to != nil {
				print(to.IsElevated())
				//print(to.IsMember())
				return
			} else {
				panic("not found")
			}
		default:
			if pi.ProcessId != 0 {
				token, err := wintoken.OpenProcessToken(int(pi.ProcessId), wintoken.TokenPrimary)
				if err == nil {
					ttt := token.Token()
					to = &ttt
				}
			}
		}
	}
}
