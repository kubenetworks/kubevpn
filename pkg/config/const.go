package config

import "os"

const (
	HOME   = ".kubevpn"
	Daemon = "daemon"

	PortPath     = "daemon_port"
	SudoPortPath = "sudo_daemon_port"

	PidPath     = "daemon_pid"
	SudoPidPath = "sudo_daemon_pid"
)

func init() {
	err := os.MkdirAll(DaemonPath, os.ModePerm)
	if err != nil {
		panic(err)
	}
}
