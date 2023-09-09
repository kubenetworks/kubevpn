package config

import "os"

const (
	HOME   = ".kubevpn"
	Daemon = "daemon"

	SockPath     = "daemon.sock"
	SudoSockPath = "sudo_daemon.sock"

	PidPath     = "daemon_pid"
	SudoPidPath = "sudo_daemon_pid"

	LogFile = "daemon.log"

	KubeVPNRestorePatchKey = "kubevpn-probe-restore-patch"
)

func init() {
	err := os.MkdirAll(DaemonPath, os.ModePerm)
	if err != nil {
		panic(err)
	}
}
