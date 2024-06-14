package config

import (
	_ "embed"
	"os"
	"path/filepath"

	"github.com/pkg/errors"
)

const (
	HOME   = ".kubevpn"
	Daemon = "daemon"

	SockPath     = "daemon.sock"
	SudoSockPath = "sudo_daemon.sock"

	PidPath     = "daemon.pid"
	SudoPidPath = "sudo_daemon.pid"

	LogFile = "daemon.log"

	KubeVPNRestorePatchKey = "kubevpn-probe-restore-patch"

	ConfigFile = "config.yaml"

	Syncthing = "syncthing"

	SyncthingGUI = "gui"

	SyncthingRemoteDeviceID = "NV72NE7-OPOUTTL-M3XV5MD-PMTWT6X-GRUE3WF-Z34YYHX-2YAOZTK-GYYDNQN"
)

//go:embed config.yaml
var config []byte

func init() {
	err := os.MkdirAll(DaemonPath, 0755)
	if err != nil {
		panic(err)
	}
	err = os.Chmod(DaemonPath, 0755)
	if err != nil {
		panic(err)
	}
	err = os.MkdirAll(PprofPath, 0755)
	if err != nil {
		panic(err)
	}
	err = os.Chmod(PprofPath, 0755)
	if err != nil {
		panic(err)
	}

	path := filepath.Join(HomePath, ConfigFile)
	_, err = os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		err = os.WriteFile(path, config, 0644)
	}
	if err != nil {
		panic(err)
	}
}
