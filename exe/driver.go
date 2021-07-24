package exe

import (
	"fmt"
	log "github.com/sirupsen/logrus"
	"os"
	"os/exec"
	"path/filepath"
)

func InstallTunTapDriver() {
	if err := Install(); err != nil {
		log.Fatal(err)
	}
}

func UninstallTunTapDriver() {
	filepath.VolumeName("C")
	path := filepath.Join(getDriver()+":\\", "Program Files", "TAP-Windows", "Uninstall.exe")
	cmd := exec.Command(path, "/S")
	b, e := cmd.CombinedOutput()
	if e != nil {
		fmt.Println(e)
	}
	fmt.Println(string(b))
}

func getDriver() string {
	for _, drive := range "ABCDEFGHIJKLMNOPQRSTUVWXYZ" {
		f, err := os.Open(string(drive) + ":\\")
		if err == nil {
			_ = f.Close()
			return string(drive)
		}
	}
	return ""
}
