package driver

import (
	"context"
	"os"
	"path/filepath"

	"k8s.io/client-go/util/retry"

	"github.com/wencaiwulue/kubevpn/v2/pkg/driver/wintun"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

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
