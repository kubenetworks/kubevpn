package driver

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"

	"k8s.io/client-go/util/retry"

	"github.com/wencaiwulue/kubevpn/v2/pkg/driver/openvpn"
	"github.com/wencaiwulue/kubevpn/v2/pkg/driver/wintun"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func InstallTunTapDriver() {
	if err := retry.OnError(retry.DefaultRetry, func(err error) bool {
		return err != nil
	}, func() error {
		return openvpn.Install()
	}); err != nil {
		plog.G(context.Background()).Warn(err)
	}
}

func InstallWireGuardTunDriver() {
	if err := retry.OnError(retry.DefaultRetry, func(err error) bool {
		return err != nil
	}, func() error {
		return wintun.InstallWintunDriver()
	}); err != nil {
		plog.G(context.Background()).Warn(err)
	}
}

func UninstallWireGuardTunDriver() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	filename := filepath.Join(filepath.Dir(executable), "wintun.dll")
	return os.Remove(filename)
}

func UninstallTunTapDriver() {
	filepath.VolumeName("C")
	path := filepath.Join(getDiskName()+":\\", "Program Files", "TAP-Windows", "Uninstall.exe")
	cmd := exec.Command(path, "/S")
	b, e := cmd.CombinedOutput()
	if e != nil {
		plog.G(context.Background()).Warn(e)
	}
	plog.G(context.Background()).Info(string(b))
}

func getDiskName() string {
	for _, drive := range "ABCDEFGHIJKLMNOPQRSTUVWXYZ" {
		f, err := os.Open(string(drive) + ":\\")
		if err == nil {
			_ = f.Close()
			return string(drive)
		}
	}
	return ""
}
