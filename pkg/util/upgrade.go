package util

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/schollz/progressbar/v3"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

var (
	address = []string{
		"https://api.github.com/repos/kubenetworks/kubevpn/releases/latest",
		"https://api.github.com/repos/wencaiwulue/kubevpn/releases/latest",
	}
	downloadAddr = "https://github.com/wencaiwulue/kubevpn/releases/latest"
)

// GetManifest fetches the latest GitHub release manifest and returns the version tag and download URL for the given OS/arch.
func GetManifest(httpCli *http.Client, goos string, arch string) (version string, downloadURL string, err error) {
	var resp *http.Response
	var errs []error
	for _, a := range address {
		resp, err = httpCli.Get(a)
		if err == nil {
			break
		}
		errs = append(errs, err)
	}
	if resp == nil {
		err = fmt.Errorf("failed to call github api: %w: %w", utilerrors.NewAggregate(errs), config.ErrUpgradeNetwork)
		return
	}
	defer resp.Body.Close()

	var content []byte
	if content, err = io.ReadAll(resp.Body); err != nil {
		err = fmt.Errorf("failed to read all response from github api: %w: %w", err, config.ErrUpgradeNetwork)
		return
	}
	var m githubRelease
	if err = json.Unmarshal(content, &m); err != nil {
		err = fmt.Errorf("failed to unmarshal response: %w: %w", err, config.ErrUpgradeNetwork)
		return
	}
	version = m.TagName
	for _, asset := range m.Assets {
		if strings.Contains(asset.Name, arch) && strings.Contains(asset.Name, goos) {
			downloadURL = asset.BrowserDownloadURL
			return
		}
	}

	// if os is not windows and darwin, default is linux
	if !sets.New[string]("windows", "darwin").Has(strings.ToLower(goos)) {
		for _, asset := range m.Assets {
			if strings.Contains(asset.Name, "linux") && strings.Contains(asset.Name, arch) {
				downloadURL = asset.BrowserDownloadURL
				return
			}
		}
	}

	err = fmt.Errorf("%s: try downloading from: %s: %w", If(m.Message != "", m.Message, string(content)), downloadAddr, config.ErrUpgradeUnsupportedPlatform)
	return
}

// Download fetches the file at the given URL, writing it to filename with a progress bar.
func Download(client *http.Client, url string, filename string, stdout, stderr io.Writer) error {
	get, err := client.Get(url)
	if err != nil {
		return err
	}
	defer get.Body.Close()
	total := float64(get.ContentLength) / 1024 / 1024
	fmt.Fprintf(stdout, "Length: %d (%0.2fM)\n", get.ContentLength, total)

	var f *os.File
	f, err = os.OpenFile(filename, os.O_RDWR|os.O_CREATE|os.O_TRUNC, config.FileModeExecutable)
	if err != nil {
		return err
	}
	defer f.Close()
	bar := progressbar.NewOptions(int(get.ContentLength),
		progressbar.OptionSetWriter(stdout),
		progressbar.OptionEnableColorCodes(true),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetWidth(25),
		progressbar.OptionOnCompletion(func() {
			_, _ = fmt.Fprint(stderr, "\n\r")
		}),
		progressbar.OptionSetRenderBlankState(true),
		progressbar.OptionSetDescription("Writing temp file..."),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "=",
			SaucerHead:    ">",
			SaucerPadding: " ",
			BarStart:      "[",
			BarEnd:        "]",
		}))
	_, err = io.Copy(io.MultiWriter(f, bar), get.Body)
	return err
}

// UnzipKubeVPNIntoFile extracts the kubevpn binary from a zip archive and writes it to filename.
func UnzipKubeVPNIntoFile(zipFile, filename string) error {
	archive, err := zip.OpenReader(zipFile)
	if err != nil {
		return err
	}
	defer archive.Close()

	var fi *zip.File
	for _, f := range archive.File {
		if strings.Contains(f.Name, "kubevpn") {
			fi = f
			break
		}
	}

	if fi == nil {
		return fmt.Errorf("cannot find kubevpn")
	}

	err = os.MkdirAll(filepath.Dir(filename), os.ModePerm)
	if err != nil {
		return err
	}

	var dstFile *os.File
	dstFile, err = os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode())
	if err != nil {
		return err
	}
	defer dstFile.Close()

	var fileInArchive io.ReadCloser
	fileInArchive, err = fi.Open()
	if err != nil {
		return err
	}
	defer fileInArchive.Close()

	_, err = io.Copy(dstFile, fileInArchive)
	return err
}

// githubRelease is the subset of the GitHub releases API response that we use.
type githubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
	Message string        `json:"message"`
}

// githubAsset is a downloadable file attached to a GitHub release.
type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}
