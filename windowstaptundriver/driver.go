package exe

import (
	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/util/retry"
	"os"
	"os/exec"
	"path/filepath"
)

func InstallTunTapDriver() {
	if err := retry.OnError(retry.DefaultRetry, func(err error) bool {
		return err != nil
	}, func() error {
		return Install()
	}); err != nil {
		log.Warn(err)
	}
}

func UninstallTunTapDriver() {
	filepath.VolumeName("C")
	path := filepath.Join(getDiskName()+":\\", "Program Files", "TAP-Windows", "Uninstall.exe")
	cmd := exec.Command(path, "/S")
	b, e := cmd.CombinedOutput()
	if e != nil {
		log.Warn(e)
	}
	log.Info(string(b))
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
