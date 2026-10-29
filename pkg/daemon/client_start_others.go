//go:build !windows

package daemon

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/elevate"
)

func startDaemonProcess(exe string, isSudo bool) (string, error) {
	output, path, err := openDaemonStartupLog(isSudo)
	if err != nil {
		return path, err
	}
	defer output.Close()
	args := []string{"daemon"}
	if isSudo {
		args = append(args, "--sudo")
		if !elevate.IsAdmin() {
			return path, elevate.RunCmdWithElevated(exe, args, output)
		}
	}
	return path, elevate.RunCmd(exe, args, output)
}

func openDaemonStartupLog(isSudo bool) (*os.File, string, error) {
	path := strings.TrimSuffix(config.GetDaemonLogPath(isSudo), ".log") + ".startup.log"
	file, err := os.CreateTemp(filepath.Dir(path), ".daemon-startup-*")
	if err != nil {
		return nil, path, err
	}
	// Replace the directory entry instead of opening a possibly root-owned file
	// or following a symlink. CreateTemp provides a new, private (0600) file.
	if err = os.Rename(file.Name(), path); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, path, err
	}
	return file, path, nil
}
