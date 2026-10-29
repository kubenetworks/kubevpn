//go:build !windows

package elevate

import (
	"context"
	"os"
	"os/exec"

	"golang.org/x/sys/unix"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// RunCmdWithElevated waits for sudo authentication and background launch, not
// daemon termination. Output must be a regular file or nil (the null device).
func RunCmdWithElevated(exe string, args []string, output *os.File) error {
	cmd := exec.Command("sudo", append([]string{"--preserve-env=HOME", "--background", exe}, args...)...)
	plog.G(context.Background()).Debug(cmd.Args)
	if output != nil {
		cmd.Stdout = output
		cmd.Stderr = output
	}
	cmd.Env = append(os.Environ(), EnvDisableSyncthingLog+"=1")
	// Nil stdin uses the null device. Sudo authenticates through /dev/tty;
	// keeping its foreground process group preserves interactive prompting.
	return cmd.Run()
}

// RunCmd starts a detached child with null stdin. Output must be a regular file
// or nil (the null device); no caller pipe may survive in the child.
func RunCmd(exe string, args []string, output *os.File) error {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &unix.SysProcAttr{
		Setpgid: true,
	}
	plog.G(context.Background()).Debug(cmd.Args)
	if output != nil {
		cmd.Stdout = output
		cmd.Stderr = output
	}
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
