package config

import (
	_ "embed"
	"os"
	"path/filepath"

	"github.com/pkg/errors"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/elevate"
)

const (
	HOME   = ".kubevpn"
	Daemon = "daemon"

	SockPath     = "daemon.sock"
	SudoSockPath = "sudo_daemon.sock"

	PidPath     = "daemon.pid"
	SudoPidPath = "sudo_daemon.pid"

	UserLogFile = "user_daemon.log"
	SudoLogFile = "root_daemon.log"

	ConfigFile = "config.yaml"

	TmpDir = "tmp"
)

//go:embed config.yaml
var config []byte

func init() {
	if elevate.IsAdmin() {
		return
	}

	var paths = []string{DaemonPath, PprofPath, GetSyncthingPath(), GetTempPath()}
	for _, path := range paths {
		err := os.MkdirAll(path, 0755)
		if err != nil {
			panic(err)
		}
		err = os.Chmod(path, 0755)
		if err != nil {
			panic(err)
		}
	}

	path := filepath.Join(HomePath, ConfigFile)
	_, err := os.Stat(path)
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

func GetConfigFilePath() string {
	return filepath.Join(HomePath, ConfigFile)
}

func GetTempPath() string {
	return filepath.Join(HomePath, TmpDir)
}
