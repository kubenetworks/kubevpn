//go:build !windows
// +build !windows

package util

import (
	log "github.com/sirupsen/logrus"
	"os"
	"os/exec"
)

//	TODO optimize while send single CRTL+C, command will quit immediately, but output will cutoff and print util quit final
func RunWithElevated() {
	cmd := exec.Command("sudo", os.Args...)
	log.Info(cmd.Args)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	err := cmd.Run()
	if err != nil {
		log.Warn(err)
	}
}

func IsAdmin() bool {
	return os.Getuid() == 0
}
