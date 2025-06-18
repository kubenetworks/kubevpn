//go:build !windows

package elevate

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func RunWithElevated() {
	cmd := exec.Command("sudo", append([]string{"--preserve-env=HOME"}, os.Args...)...)
	plog.G(context.Background()).Debug(cmd.Args)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(), EnvDisableSyncthingLog+"=1")
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
