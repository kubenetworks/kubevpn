//go:build !windows

package elevate

import (
	"context"
	"os"
	"os/exec"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"golang.org/x/sys/unix"
)

func RunCmdWithElevated(exe string, args []string) error {
	cmd := exec.Command("sudo", append([]string{"--preserve-env=HOME", "--background", exe}, args...)...)
	plog.G(context.Background()).Debug(cmd.Args)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(), EnvDisableSyncthingLog+"=1")
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
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &unix.SysProcAttr{
		Setpgid: true,
	}
	plog.G(context.Background()).Debug(cmd.Args)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(), EnvDisableSyncthingLog+"=1")
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
