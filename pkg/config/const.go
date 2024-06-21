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

func GetSockPath(isSudo bool) string {
	name := SockPath
	if isSudo {
		name = SudoSockPath
	}
	return filepath.Join(DaemonPath, name)
}

func GetPidPath(isSudo bool) string {
	name := PidPath
	if isSudo {
		name = SudoPidPath
	}
	return filepath.Join(DaemonPath, name)
}

func GetSyncthingPath() string {
	return filepath.Join(DaemonPath, SyncthingDir)
}

func GetSyncthingGUIPath() string {
	return filepath.Join(DaemonPath, SyncthingDir, SyncthingGUIDir)
}

func GetConfigFilePath() string {
	return filepath.Join(HomePath, ConfigFile)
}
