package util

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
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

// nameHasToken reports whether token appears as a whole segment of name, where segments
// are separated by any of '_', '.', or '-' (case-insensitive). Release asset names look
// like "kubevpn_v2.0.0_linux_amd64.zip", so the OS/arch are underscore-delimited tokens.
//
// It replaces strings.Contains for matching arch: arch="arm" must NOT match the "arm64"
// segment in "kubevpn_v2.0.0_linux_arm64.zip" (a substring Contains would), or a 32-bit
// ARM user would download the arm64 binary and fail to run it. The same applies to OS
// tokens, which are also matched as whole segments for consistency.
func nameHasToken(name, token string) bool {
	for _, seg := range strings.FieldsFunc(name, func(r rune) bool {
		return r == '_' || r == '.' || r == '-'
	}) {
		if strings.EqualFold(seg, token) {
			return true
		}
	}
	return false
}

// GetManifest fetches the latest GitHub release manifest and returns the version tag and download URL for the given OS/arch.
func GetManifest(httpCli *http.Client, goos string, arch string) (version string, downloadURL string, err error) {
	var resp *http.Response
	var errs []error
	for _, a := range address {
		resp, err = httpCli.Get(a)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if resp.StatusCode == http.StatusOK {
			break
		}
		// A non-200 (rate-limit 403, 404, ...) still returns a body; do not parse it
		// as a release manifest — record and try the next mirror.
		errs = append(errs, fmt.Errorf("github api %s: status %d", a, resp.StatusCode))
		_ = resp.Body.Close()
		resp = nil
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
		if nameHasToken(asset.Name, arch) && nameHasToken(asset.Name, goos) {
			downloadURL = asset.BrowserDownloadURL
			return
		}
	}

	// if os is not windows and darwin, default is linux
	if !sets.New[string]("windows", "darwin").Has(strings.ToLower(goos)) {
		for _, asset := range m.Assets {
			if nameHasToken(asset.Name, "linux") && nameHasToken(asset.Name, arch) {
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
	if get.StatusCode != http.StatusOK {
		// Without this, a 404/403 error page would be written out and later fail
		// deep in unzip with a misleading "not a valid zip" error.
		return fmt.Errorf("download %s failed, status code: %d", url, get.StatusCode)
	}
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
// If the archive also bundles a checksums.txt (SHA-256 of the binary), the extracted binary is
// verified against it. NOTE: this only detects a corrupt/truncated download — the checksum ships
// inside the same zip, so it is NOT tamper-proof; real supply-chain integrity needs an externally
// published (ideally signed) checksum. See docs/33-client-upgrade.md.
func UnzipKubeVPNIntoFile(zipFile, filename string) error {
	archive, err := zip.OpenReader(zipFile)
	if err != nil {
		return err
	}
	defer archive.Close()

	var fi, sumEntry *zip.File
	for _, f := range archive.File {
		switch {
		case filepath.Base(f.Name) == "checksums.txt":
			sumEntry = f
		case fi == nil && strings.Contains(f.Name, "kubevpn"):
			fi = f
		}
	}

	if fi == nil {
		return fmt.Errorf("cannot find kubevpn")
	}

	err = os.MkdirAll(filepath.Dir(filename), config.FileModeExecutable)
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

	hash := sha256.New()
	if _, err = io.Copy(io.MultiWriter(dstFile, hash), fileInArchive); err != nil {
		return err
	}
	if sumEntry != nil {
		if err = verifyBundledChecksum(sumEntry, hash.Sum(nil)); err != nil {
			return err
		}
	}
	return nil
}

// verifyBundledChecksum compares the extracted binary's SHA-256 against the checksums.txt entry
// (which contains the hex digest, optionally followed by a filename in `shasum` format).
func verifyBundledChecksum(sumEntry *zip.File, got []byte) error {
	rc, err := sumEntry.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	want := strings.TrimSpace(string(data))
	if i := strings.IndexAny(want, " \t"); i > 0 { // tolerate "<hash>  <filename>"
		want = want[:i]
	}
	if want == "" {
		return nil // empty checksums.txt: nothing to verify against
	}
	if gotHex := hex.EncodeToString(got); !strings.EqualFold(want, gotHex) {
		return fmt.Errorf("kubevpn binary checksum mismatch: want %s, got %s", want, gotHex)
	}
	return nil
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
