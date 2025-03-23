//go:build !windows

package elevate

import (
	"context"
	"flag"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"

	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

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
	cmd.Env = append(os.Environ(), config.EnvDisableSyncthingLog+"=1")
	// while send single CTRL+C, command will quit immediately, but output will cut off and print util quit final
	// so, mute single CTRL+C, let inner command handle single only
	go func() {
		signals := make(chan os.Signal)
		signal.Notify(signals, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL, syscall.SIGSTOP)
		<-signals
	}()
	err := cmd.Run()
	if err != nil {
		plog.G(context.Background()).Warn(err)
	}
}

func IsAdmin() bool {
	/*_, ok := os.LookupEnv(config.EnvStartSudoKubeVPNByKubeVPN)
	if os.Getuid() == 0 {
		if !ok {
			strings := []string{
				"Warn: Use sudo to execute command kubevpn can not use user env KUBECONFIG.",
				"Because of sudo user env and user env are different.",
				"Current env KUBECONFIG value: " + os.Getenv(clientcmd.RecommendedConfigPathEnvVar),
			}
			PrintLine(nil, strings...)
			for i := 0; i >= 0; i-- {
				_, _ = fmt.Printf("\r %ds", i)
				time.Sleep(time.Second * 1)
			}
			_, _ = fmt.Printf("\r")
		}
	}*/
	return os.Getuid() == 0
}
