package driver

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"testing"
)

func TestInstall(t *testing.T) {
	InstallTunTapDriver()
}

func TestUninstall(t *testing.T) {
	UninstallTunTapDriver()
}
func TestAA(t *testing.T) {
	cmd := exec.Command("vpn", "version")
	b, e := cmd.CombinedOutput()
	if e != nil {
		fmt.Println(e)
	}
	fmt.Println(string(b))
}

func TestName(t *testing.T) {
	tempFile, _ := ioutil.TempFile("", "*.exe")
	fmt.Println(tempFile.Name())
	_ = os.Remove(tempFile.Name())
}
