//go:build !windows

package util

import (
	"flag"
	"os"
	"os/exec"
	"runtime"

	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func RunCmdWithElevated(exe string, args []string) error {
	// fix if startup with normal user, after elevated home dir will change to root user in linux
	// but unix don't have this issue
	if runtime.GOOS == "linux" && flag.Lookup("kubeconfig") == nil {
		if _, err := os.Stat(clientcmd.RecommendedHomeFile); err == nil {
			os.Args = append(os.Args, "--kubeconfig", clientcmd.RecommendedHomeFile)
		}
	}
	cmd := exec.Command("sudo", append([]string{"--preserve-env", exe}, args...)...)
	log.Debug(cmd.Args)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(), config.EnvStartSudoKubeVPNByKubeVPN+"=1", config.EnvDisableSyncthingLog+"=1")
	err := cmd.Start()
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

func RunCmd(exe string, args []string) error {
	// fix if startup with normal user, after elevated home dir will change to root user in linux
	// but unix don't have this issue
	if runtime.GOOS == "linux" && flag.Lookup("kubeconfig") == nil {
		if _, err := os.Stat(clientcmd.RecommendedHomeFile); err == nil {
			os.Args = append(os.Args, "--kubeconfig", clientcmd.RecommendedHomeFile)
		}
	}
	cmd := exec.Command(exe, args...)
	log.Debug(cmd.Args)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(), config.EnvStartSudoKubeVPNByKubeVPN+"=1", config.EnvDisableSyncthingLog+"=1")
	err := cmd.Start()
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
