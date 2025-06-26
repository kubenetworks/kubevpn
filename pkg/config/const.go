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
	Log    = "log"

	SockPath     = "user_daemon.sock"
	SudoSockPath = "root_daemon.sock"

	PidPath     = "user_daemon.pid"
	SudoPidPath = "root_daemon.pid"

	UserLogFile = "user_daemon.log"
	SudoLogFile = "root_daemon.log"

	ConfigFile = "config.yaml"

	TempDir = "temp"

	DBFile = "db"
)

var (
	homePath   string
	daemonPath string
	logPath    string

	//go:embed config.yaml
	config []byte
)

func init() {
	dir, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	homePath = filepath.Join(dir, HOME)
	daemonPath = filepath.Join(dir, HOME, Daemon)
	logPath = filepath.Join(dir, HOME, Log)

	var paths = []string{homePath, daemonPath, logPath, GetPProfPath(), GetSyncthingPath(), GetTempPath()}
	for _, path := range paths {
		_, err = os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			err = os.MkdirAll(path, 0755)
			if err != nil {
				panic(err)
			}
			err = os.Chmod(path, 0755)
			if err != nil {
				panic(err)
			}
		} else if err != nil {
			panic(err)
		}
	}

	path := filepath.Join(homePath, ConfigFile)
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
	return filepath.Join(daemonPath, name)
}

func GetPidPath(isSudo bool) string {
	name := PidPath
	if isSudo {
		name = SudoPidPath
	}
	return filepath.Join(daemonPath, name)
}

func GetSyncthingPath() string {
	return filepath.Join(daemonPath, SyncthingDir)
}

func GetConfigFile() string {
	return filepath.Join(homePath, ConfigFile)
}

func GetTempPath() string {
	return filepath.Join(homePath, TempDir)
}

func GetDaemonLogPath(isSudo bool) string {
	if isSudo {
		return filepath.Join(logPath, SudoLogFile)
	}
	return filepath.Join(logPath, UserLogFile)
}

func GetPProfPath() string {
	return filepath.Join(daemonPath, PProfDir)
}

func GetDBPath() string {
	return filepath.Join(daemonPath, DBFile)
}
