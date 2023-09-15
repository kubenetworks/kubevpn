//go:build !windows
// +build !windows

package util

import (
	"flag"
	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/tools/clientcmd"
	"os"
	"os/exec"
	"runtime"
)

func RunCmdWithElevated(args []string) error {
	// fix if startup with normal user, after elevated home dir will change to root user in linux
	// but unix don't have this issue
	if runtime.GOOS == "linux" && flag.Lookup("kubeconfig") == nil {
		if _, err := os.Stat(clientcmd.RecommendedHomeFile); err == nil {
			os.Args = append(os.Args, "--kubeconfig", clientcmd.RecommendedHomeFile)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command("sudo", append([]string{"--preserve-env", exe}, args...)...)
	log.Debug(cmd.Args)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(), envStartSudoKubeVPNByKubeVPN+"=1")
	//// while send single CTRL+C, command will quit immediately, but output will cut off and print util quit final
	//// so, mute single CTRL+C, let inner command handle single only
	//go func() {
	//	signals := make(chan os.Signal)
	//	signal.Notify(signals, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL, syscall.SIGSTOP)
	//	<-signals
	//}()
	err = cmd.Start()
	if err != nil {
		return err
	}
	go func() {
		err := cmd.Wait()
		if err != nil {
			log.Warn(err)
		}
	}()
	return nil
}

func RunCmd(args []string) error {
	// fix if startup with normal user, after elevated home dir will change to root user in linux
	// but unix don't have this issue
	if runtime.GOOS == "linux" && flag.Lookup("kubeconfig") == nil {
		if _, err := os.Stat(clientcmd.RecommendedHomeFile); err == nil {
			os.Args = append(os.Args, "--kubeconfig", clientcmd.RecommendedHomeFile)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, args...)
	log.Debug(cmd.Args)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(), envStartSudoKubeVPNByKubeVPN+"=1")
	//// while send single CTRL+C, command will quit immediately, but output will cut off and print util quit final
	//// so, mute single CTRL+C, let inner command handle single only
	//go func() {
	//	signals := make(chan os.Signal)
	//	signal.Notify(signals, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL, syscall.SIGSTOP)
	//	<-signals
	//}()
	err = cmd.Start()
	if err != nil {
		return err
	}
	go func() {
		err := cmd.Wait()
		if err != nil {
			log.Warn(err)
		}
	}()
	return nil
}
