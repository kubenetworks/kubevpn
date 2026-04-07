//go:build windows

package cmds

import "os/exec"

func detachProcess(cmd *exec.Cmd) {}
