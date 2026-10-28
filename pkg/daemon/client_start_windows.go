//go:build windows

package daemon

import "github.com/wencaiwulue/kubevpn/v2/pkg/daemon/elevate"

func startDaemonProcess(exe string, isSudo bool) (string, error) {
	args := []string{"daemon"}
	if isSudo {
		args = append(args, "--sudo")
		if !elevate.IsAdmin() {
			return "", elevate.RunCmdWithElevated(exe, args, nil)
		}
	}
	return "", elevate.RunCmd(exe, args, nil)
}
