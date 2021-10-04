//go:build !windows

package util

import (
	log "github.com/sirupsen/logrus"
	"os"
	"os/exec"
)

func RunWithElevated() {
	command := exec.Command("sudo", os.Args...)
	log.Info(command.Args)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Stdin = os.Stdin
	err := command.Run()
	if err != nil {
		log.Warn(err)
	}
}

func IsAdmin() bool {
	return os.Getuid() == 0
}
