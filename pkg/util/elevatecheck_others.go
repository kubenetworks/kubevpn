//go:build !windows

package util

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

const envStartSudoKubeVPNByKubeVPN = config.EnvStartSudoKubeVPNByKubeVPN

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
	cmd.Env = append(os.Environ(), envStartSudoKubeVPNByKubeVPN+"=1", config.EnvDisableSyncthingLog+"=1")
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
			fmt.Println()
			fmt.Println(`----------------------------------------------------------------------------------`)
			fmt.Println(`    Warn: Use sudo to execute command kubevpn can not use user env KUBECONFIG.    `)
			fmt.Println(`    Because of sudo user env and user env are different.    `)
			fmt.Println(`    Current env KUBECONFIG value: ` + os.Getenv(clientcmd.RecommendedConfigPathEnvVar))
			fmt.Println(`----------------------------------------------------------------------------------`)
			fmt.Println()
			for i := 0; i >= 0; i-- {
				_, _ = fmt.Printf("\r %ds", i)
				time.Sleep(time.Second * 1)
			}
			_, _ = fmt.Printf("\r")
		}
	}
	return os.Getuid() == 0
}
