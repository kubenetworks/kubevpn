//go:build !windows
// +build !windows

package util

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/tools/clientcmd"
)

const envStartSudoKubeVPNByKubeVPN = "DEPTH_SIGNED_BY_NAISON"

func RunWithElevated() {
	// fix if startup with normal user, after elevated home dir will change to root user in linux
	// but unix don't have this issue
	if runtime.GOOS == "linux" && flag.Lookup("kubeconfig") == nil {
		if _, err := os.Stat(clientcmd.RecommendedHomeFile); err == nil {
			os.Args = append(os.Args, "--kubeconfig", clientcmd.RecommendedHomeFile)
		}
	}
	cmd := exec.Command("sudo", append([]string{"--preserve-env"}, os.Args...)...)
	log.Debug(cmd.Args)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(), envStartSudoKubeVPNByKubeVPN+"=1")
	// while send single CTRL+C, command will quit immediately, but output will cut off and print util quit final
	// so, mute single CTRL+C, let inner command handle single only
	go func() {
		signals := make(chan os.Signal)
		signal.Notify(signals, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL, syscall.SIGSTOP)
		<-signals
	}()
	err := cmd.Run()
	if err != nil {
		log.Warn(err)
	}
}

func IsAdmin() bool {
	_, ok := os.LookupEnv(envStartSudoKubeVPNByKubeVPN)
	if os.Getuid() == 0 {
		if !ok {
			s := "Error: Please do not use sudo to execute command kubevpn, try to use command: \n" + strings.Join(os.Args, " ")
			fmt.Println(s)
			os.Exit(1)
		}
	}
	return os.Getuid() == 0
}
