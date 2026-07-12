package config

import (
	_ "embed"
	"errors"
	"os"
	"path/filepath"
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

// File permission modes for files, directories, and sockets created by kubevpn.
const (
	// FileModeFile is the mode for regular files: rw-r--r--.
	FileModeFile os.FileMode = 0644
	// FileModeExecutable is the mode for executables and directories: rwxr-xr-x.
	FileModeExecutable os.FileMode = 0755
	// FileModePrivate is the mode for owner-only files/directories: rwx------.
	FileModePrivate os.FileMode = 0700
	// FileModeSocket is the mode for the daemon unix socket: rw-rw-rw-.
	FileModeSocket os.FileMode = 0666
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

	// These dirs are shared by the user (unprivileged) and sudo (root) daemons,
	// which run with the same HOME. They must be created by the user daemon FIRST
	// so they are owned by the unprivileged user; the sudo daemon then only Stats
	// them. StartupDaemon enforces that start order — see pkg/daemon/client.go.
	paths := []string{homePath, daemonPath, logPath, GetPProfPath(), GetSyncthingPath(), GetTempPath()}
	for _, path := range paths {
		_, err = os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			if err = os.MkdirAll(path, FileModeExecutable); err != nil {
				panic(err)
			}
		} else if err != nil {
			panic(err)
		}
	}

	path := filepath.Join(homePath, ConfigFile)
	_, err = os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		err = os.WriteFile(path, config, FileModeFile)
	}
	if err != nil {
		panic(err)
	}
}

// GetSockPath returns the Unix domain socket path for the user or root daemon.
func GetSockPath(isSudo bool) string {
	name := SockPath
	if isSudo {
		name = SudoSockPath
	}
	return filepath.Join(daemonPath, name)
}

// GetPidPath returns the PID file path for the user or root daemon.
func GetPidPath(isSudo bool) string {
	name := PidPath
	if isSudo {
		name = SudoPidPath
	}
	return filepath.Join(daemonPath, name)
}

// GetSyncthingPath returns the directory path for Syncthing runtime data.
func GetSyncthingPath() string {
	return filepath.Join(daemonPath, SyncthingDir)
}

// GetConfigFile returns the path to the kubevpn YAML configuration file.
func GetConfigFile() string {
	return filepath.Join(homePath, ConfigFile)
}

// GetTempPath returns the directory path for kubevpn temporary files.
func GetTempPath() string {
	return filepath.Join(homePath, TempDir)
}

// GetDaemonLogPath returns the log file path for the user or root daemon.
func GetDaemonLogPath(isSudo bool) string {
	if isSudo {
		return filepath.Join(logPath, SudoLogFile)
	}
	return filepath.Join(logPath, UserLogFile)
}

// GetPProfPath returns the directory path for storing pprof profile data.
func GetPProfPath() string {
	return filepath.Join(daemonPath, PProfDir)
}

// GetDBPath returns the file path for the local embedded database.
func GetDBPath() string {
	return filepath.Join(daemonPath, DBFile)
}
