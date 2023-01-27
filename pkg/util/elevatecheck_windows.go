//go:build windows
// +build windows

package util

import (
	"context"
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/pkg/util/wintoken"
	"golang.org/x/sys/windows"
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

	//token, err := wintoken.OpenProcessToken(1234, wintoken.TokenPrimary)
	//if err != nil {
	//	panic(err)
	//}
	inner := RunWithElevatedInner()
	if inner == 0 {
		panic(inner)
	}
	//err = token.EnableAllPrivileges()
	//if err != nil {
	//	panic(err)
	//}

	//defer token.Close()
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Token: syscall.Token(inner),
	}
	// while send single CTRL+C, command will quit immediately, but output will cut off and print util quit final
	// so, mute single CTRL+C, let inner command handle single only
	go func() {
		signals := make(chan os.Signal)
		signal.Notify(signals, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL)
		<-signals
	}()
	err := cmd.Run()
	if err != nil {
		log.Warn(err)
	}
}

func RunWithElevatedInner() windows.Token {
	verb := "runas"
	exe, _ := os.Executable()
	//cwd, _ := os.Getwd()
	//args := /* strings.Join(os.Args[:], " ")*/ ""

	//verbPtr, _ := windows.UTF16PtrFromString(verb)
	//exePtr, _ := syscall.UTF16PtrFromString(exe)
	//cwdPtr, _ := syscall.UTF16PtrFromString(cwd)
	//argPtr, _ := syscall.UTF16PtrFromString(args)

	// https://learn.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-showwindow
	//var showCmd int32 = 1 //SW_NORMAL
	//process := windows.CurrentProcess()
	//process := windows.GetDesktopWindow()
	//windows.Setenv("KUBECONFIG", os.Getenv("KUBECONFIG"))
	//v, _ := syscall.UTF16PtrFromString("KUBECONFIG=" + os.Getenv("KUBECONFIG"))
	//err := windows.ShellExecute(0, verbPtr, exePtr, argPtr, cwdPtr, showCmd)
	//if err != nil {
	//	logrus.Warn(err)
	//}
	// runas /env /user:administrator "kubevpn-windows-amd64.exe"
	strings := []string{"/env", "/user:administrator", fmt.Sprintf(`"%s version"`, exe)}
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
	var tt *windows.Token
	for {
		select {
		case <-cancel.Done():
			if tt != nil {
				return *tt
			}
			return 0
		default:
			token, err := wintoken.OpenProcessToken(cmd.Process.Pid, wintoken.TokenPrimary)
			if err == nil {
				ttt := token.Token()
				tt = &ttt
			}
		}
	}
}

func IsAdmin() bool {
	_, err := os.Open("\\\\.\\PHYSICALDRIVE0")
	if err != nil {
		return false
	}
	return true
}
