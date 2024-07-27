package upgrade

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	goversion "github.com/hashicorp/go-version"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/elevate"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// Main
// 1) get current binary version
// 2) get the latest version
// 3) compare two version decide needs to download or not
// 4) download newer version zip
// 5) unzip to temp file
// 6) check permission of putting new kubevpn back
// 7) chmod +x, move old to /temp, move new to CURRENT_FOLDER
func Main(ctx context.Context, client *http.Client, url string) error {
	err := elevatePermission()
	if err != nil {
		return err
	}

	err = downloadAndInstall(client, url)
	if err != nil {
		return err
	}

	fmt.Fprint(os.Stdout, "Upgrade daemon...\n")
	err = daemon.StartupDaemon(context.Background())
	fmt.Fprint(os.Stdout, "Done\n")
	return err
}

func downloadAndInstall(client *http.Client, url string) error {
	temp, err := os.CreateTemp("", "")
	if err != nil {
		return err
	}
	err = temp.Close()
	if err != nil {
		return err
	}
	err = util.Download(client, url, temp.Name(), os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	file, _ := os.CreateTemp("", "")
	err = file.Close()
	if err != nil {
		return err
	}
	err = util.UnzipKubeVPNIntoFile(temp.Name(), file.Name())
	if err != nil {
		return err
	}
	err = os.Chmod(file.Name(), 0755)
	if err != nil {
		return err
	}
	var curFolder string
	curFolder, err = os.Executable()
	if err != nil {
		return err
	}
	var createTemp *os.File
	createTemp, err = os.CreateTemp("", "")
	if err != nil {
		return err
	}
	err = createTemp.Close()
	if err != nil {
		return err
	}
	err = os.Remove(createTemp.Name())
	if err != nil {
		return err
	}
	err = util.Move(curFolder, createTemp.Name())
	if err != nil {
		return err
	}
	err = util.Move(file.Name(), curFolder)
	return err
}

func elevatePermission() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	var tem *os.File
	tem, err = os.Create(filepath.Join(filepath.Dir(executable), ".test"))
	if tem != nil {
		_ = tem.Close()
		_ = os.Remove(tem.Name())
	}
	if os.IsPermission(err) {
		elevate.RunWithElevated()
		os.Exit(0)
	} else if err != nil {
		return err
	} else if !elevate.IsAdmin() {
		elevate.RunWithElevated()
		os.Exit(0)
	}
	return nil
}

func NeedsUpgrade(ctx context.Context, client *http.Client, version string) (url string, latestVersion string, upgrade bool, err error) {
	latestVersion, url, err = util.GetManifest(client, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		v := "https://github.com/kubenetworks/kubevpn/raw/master/plugins/stable.txt"
		var stream []byte
		stream, err = util.DownloadFileStream(v)
		latestVersion = strings.TrimSpace(string(stream))
		url = fmt.Sprintf("https://github.com/kubenetworks/kubevpn/releases/download/%s/kubevpn_%s_%s_%s.zip", latestVersion, latestVersion, runtime.GOOS, runtime.GOARCH)
	}
	if err != nil {
		return
	}

	var currV, latestV *goversion.Version
	currV, err = goversion.NewVersion(version)
	if err != nil {
		return
	}
	latestV, err = goversion.NewVersion(latestVersion)
	if err != nil {
		return
	}
	if currV.GreaterThanOrEqual(latestV) {
		return
	}
	upgrade = true
	return
}
