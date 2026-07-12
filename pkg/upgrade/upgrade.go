package upgrade

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

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

	const httpClientTimeout = 30 * time.Second
	client := &http.Client{Timeout: httpClientTimeout}
	if config.GitHubOAuthToken != "" {
		client = oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: config.GitHubOAuthToken, TokenType: "Bearer"}))
		// oauth2.NewClient returns a client with no Timeout; re-apply it so an
		// authenticated request cannot hang the upgrade forever.
		client.Timeout = httpClientTimeout
	}
	url, latestVersion, needsUpgrade, err := needsUpgrade(ctx, client, config.Version)
	if err != nil {
		return err
	}
	if !needsUpgrade {
		fmt.Fprintf(os.Stdout, "Already up to date, no need to upgrade, version: %s\n", latestVersion)
		return nil
	}
	fmt.Fprintf(os.Stdout, "Current version: %s less than latest version: %s, needs to upgrade\n", config.Version, latestVersion)
	// Best-effort: stop both daemons so they do not hold the old binary open while
	// we replace it. Failures are logged but do not block the upgrade.
	if err = quit(ctx, true); err != nil {
		plog.G(ctx).Debugf("Failed to quit sudo daemon before upgrade: %v", err)
	}
	if err = quit(ctx, false); err != nil {
		plog.G(ctx).Debugf("Failed to quit user daemon before upgrade: %v", err)
	}

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
		return fmt.Errorf("download release: %w: %w", err, config.ErrUpgradeNetwork)
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
		return fmt.Errorf("unzip release: %w: %w", err, config.ErrUpgradeInstall)
	}
	err = os.Chmod(file.Name(), config.FileModeExecutable)
	if err != nil {
		return fmt.Errorf("chmod new binary: %w: %w", err, config.ErrUpgradeInstall)
	}

	var createTemp *os.File
	createTemp, err = os.CreateTemp(filepath.Dir(curFolder), "")
	if err != nil {
		return err
	}
	err = createTemp.Close()
	if err != nil {
		return err
	}
	backupPath := createTemp.Name()
	err = os.Remove(backupPath)
	if err != nil {
		return err
	}
	// Stash the current binary as a backup. Renaming the running executable away
	// first (rather than overwriting it in place) is required on Windows, where the
	// running .exe is locked and cannot be opened for writing — but rename is allowed.
	err = util.Move(curFolder, backupPath)
	if err != nil {
		return fmt.Errorf("backup current binary: %w: %w", err, config.ErrUpgradeInstall)
	}
	// Install the new binary; if it fails, restore the backup so the user is
	// never left without a kubevpn binary on disk.
	err = util.Move(file.Name(), curFolder)
	if err != nil {
		_ = util.Move(backupPath, curFolder)
		return fmt.Errorf("install new binary: %w: %w", err, config.ErrUpgradeInstall)
	}
	// Success: drop the backup.
	_ = os.Remove(backupPath)
	return nil
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
	}
	return err
}

func needsUpgrade(ctx context.Context, client *http.Client, version string) (url string, latestVersion string, upgrade bool, err error) {
	latestVersion, url, err = util.GetManifest(client, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		// Fallback: read the pinned latest version from stable.txt and build the
		// release URL by convention. Surface the fallback's own error immediately
		// instead of letting it shadow into a confusing version-parse failure.
		v := "https://github.com/kubenetworks/kubevpn/raw/master/plugins/stable.txt"
		var stream []byte
		stream, err = util.DownloadFileStream(v)
		if err != nil {
			err = fmt.Errorf("determine latest version: %w: %w", err, config.ErrUpgradeNetwork)
			return
		}
		latestVersion = strings.TrimSpace(string(stream))
		if latestVersion == "" {
			err = fmt.Errorf("determine latest version: empty response from %s: %w", v, config.ErrUpgradeNetwork)
			return
		}
		url = fmt.Sprintf("https://github.com/kubenetworks/kubevpn/releases/download/%s/kubevpn_%s_%s_%s.zip", latestVersion, latestVersion, runtime.GOOS, runtime.GOARCH)
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
