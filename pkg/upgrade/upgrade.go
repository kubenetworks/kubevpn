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
	"golang.org/x/oauth2"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/elevate"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
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
func Main(ctx context.Context, quit func(ctx context.Context, isSudo bool) error) error {
	err := elevatePermission()
	if err != nil {
		return err
	}

	var client = http.DefaultClient
	if config.GitHubOAuthToken != "" {
		client = oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: config.GitHubOAuthToken, TokenType: "Bearer"}))
	}
	url, latestVersion, needsUpgrade, err := NeedsUpgrade(ctx, client, config.Version)
	if err != nil {
		return err
	}
	if !needsUpgrade {
		_, _ = fmt.Fprintln(os.Stdout, fmt.Sprintf("Already up to date, don't needs to upgrade, version: %s", latestVersion))
		return nil
	}
	_, _ = fmt.Fprintln(os.Stdout, fmt.Sprintf("Current version: %s less than latest version: %s, needs to upgrade", config.Version, latestVersion))
	_ = quit(ctx, true)
	_ = quit(ctx, false)

	err = downloadAndInstall(client, url)
	if err != nil {
		return err
	}

	plog.G(ctx).Infof("Upgrade daemon...")
	_ = daemon.StartupDaemon(context.Background())
	plog.G(ctx).Info("Done")
	return nil
}

func downloadAndInstall(client *http.Client, url string) error {
	temp, err := os.CreateTemp("", "*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	err = temp.Close()
	if err != nil {
		return err
	}
	err = util.Download(client, url, temp.Name(), os.Stdout, os.Stderr)
	if err != nil {
		return err
	}

	var curFolder string
	curFolder, err = os.Executable()
	if err != nil {
		return err
	}
	var file *os.File
	file, err = os.CreateTemp(filepath.Dir(curFolder), "")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
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

	var createTemp *os.File
	createTemp, err = os.CreateTemp(filepath.Dir(curFolder), "")
	if err != nil {
		return err
	}
	defer os.Remove(createTemp.Name())
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
	if err == nil {
		return nil
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
