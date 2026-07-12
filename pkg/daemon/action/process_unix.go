//go:build !windows

package action

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// sudoProcessAliveImpl reports whether the root (sudo) daemon process named by its PID file is
// currently running. It is a cheap, crash-safe liveness pre-check: signal 0 delivers nothing
// but checks the process's existence/permission, so it detects a kill -9'd daemon that left a
// stale socket file behind (which a plain os.Stat on the socket would not).
func sudoProcessAliveImpl() bool {
	b, err := os.ReadFile(config.GetPidPath(true))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// A live process either accepts signal 0 (nil) or rejects it with EPERM (exists, not ours).
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission)
}
