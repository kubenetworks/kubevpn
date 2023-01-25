//go:build windows
// +build windows

package util

import (
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/pkg/util/wintoken"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// ref https://stackoverflow.com/questions/31558066/how-to-ask-for-administer-privileges-on-windows-with-go
func RunWithElevated() {
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "connect")
	log.Debug(cmd.Args)
	fmt.Println(cmd.Args)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(), "KUBECONFIG="+os.Getenv("KUBECONFIG"))
	//token, err := wintoken.GetInteractiveToken(wintoken.TokenLinked)
	//if err != nil {
	//	panic(err)
	//}
	token, err := wintoken.OpenProcessToken(0, wintoken.TokenPrimary)
	if err != nil {
		panic(err)
	}
	err = token.EnableAllPrivileges()
	if err != nil {
		panic(err)
	}

	defer token.Close()
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Token: syscall.Token(token.Token()),
	}
	// while send single CTRL+C, command will quit immediately, but output will cut off and print util quit final
	// so, mute single CTRL+C, let inner command handle single only
	go func() {
		signals := make(chan os.Signal)
		signal.Notify(signals, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL)
		<-signals
	}()
	err = cmd.Run()
	if err != nil {
		log.Warn(err)
	}
}

//func RunWithElevated() {
//	verb := "runas"
//	//exe, _ := os.Executable()
//	cwd, _ := os.Getwd()
//	args := strings.Join(os.Args[:], " ")
//
//	//verbPtr, _ := windows.UTF16PtrFromString(verb)
//	//exePtr, _ := syscall.UTF16PtrFromString(exe)
//	cwdPtr, _ := syscall.UTF16PtrFromString(cwd)
//	argPtr, _ := syscall.UTF16PtrFromString(args)
//
//	// https://learn.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-showwindow
//	//var showCmd int32 = 1 //SW_NORMAL
//	//process := windows.CurrentProcess()
//	//process := windows.GetDesktopWindow()
//	//windows.Setenv("KUBECONFIG", os.Getenv("KUBECONFIG"))
//	v, _ := syscall.UTF16PtrFromString("KUBECONFIG=" + os.Getenv("KUBECONFIG"))
//
//	var si windows.StartupInfo
//	var pi windows.ProcessInformation
////windows.TokenElevation
//	// CreateProcess(NULL,"cleanmgr",NULL,NULL,FALSE,NORMAL_PRIORITY_CLASS,NULL,NULL,&si,&pi); //调用系统的清理磁盘程序
//	err := windows.CreateProcessAsUser(nil,nil, argPtr, nil, nil, true, windows.NORMAL_PRIORITY_CLASS, v, cwdPtr, &si, &pi)
//	//r1, _, e1 := syscall.Syscall6(procShellExecuteW.Addr(), 6, uintptr(hwnd), uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)), uintptr(unsafe.Pointer(args)), uintptr(unsafe.Pointer(cwd)), uintptr(showCmd))
//	//r1, _, e1 := syscall.Syscall12(procCreateProcessW.Addr(), 10, uintptr(unsafe.Pointer(appName)), uintptr(unsafe.Pointer(commandLine)), uintptr(unsafe.Pointer(procSecurity)), uintptr(unsafe.Pointer(threadSecurity)), uintptr(_p0), uintptr(creationFlags), uintptr(unsafe.Pointer(env)), uintptr(unsafe.Pointer(currentDir)), uintptr(unsafe.Pointer(startupInfo)), uintptr(unsafe.Pointer(outProcInfo)), 0, 0)
//	//err := windows.ShellExecute(windows.Handle(process), verbPtr, exePtr, argPtr, cwdPtr, showCmd)
//	if err != nil {
//		logrus.Warn(err)
//	}
//}

func IsAdmin() bool {
	_, err := os.Open("\\\\.\\PHYSICALDRIVE0")
	if err != nil {
		return false
	}
	return true
}
