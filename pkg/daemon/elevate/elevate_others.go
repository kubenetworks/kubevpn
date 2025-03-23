//go:build !windows

package elevate

import (
	"context"
	"flag"
	"os"
	"os/exec"
	"runtime"

	"golang.org/x/sys/unix"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func RunCmdWithElevated(exe string, args []string) error {
	// fix if startup with normal user, after elevated home dir will change to root user in linux
	// but unix don't have this issue
	if runtime.GOOS == "linux" && flag.Lookup("kubeconfig") == nil {
		if _, err := os.Stat(clientcmd.RecommendedHomeFile); err == nil {
			os.Args = append(os.Args, "--kubeconfig", clientcmd.RecommendedHomeFile)
		}
	}
	cmd := exec.Command("sudo", append([]string{"--preserve-env", "--background", exe}, args...)...)
	plog.G(context.Background()).Debug(cmd.Args)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(), config.EnvDisableSyncthingLog+"=1")
	err := cmd.Start()
	if err != nil {
		return err
	}
	err = cmd.Process.Release()
	if err != nil {
		return err
	}
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
	cmd.SysProcAttr = &unix.SysProcAttr{
		Setpgid: true,
	}
	plog.G(context.Background()).Debug(cmd.Args)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(), config.EnvDisableSyncthingLog+"=1")
	err := cmd.Start()
	if err != nil {
		return err
	}
	err = cmd.Process.Release()
	if err != nil {
		return err
	}
	return nil
}
