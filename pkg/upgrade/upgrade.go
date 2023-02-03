package upgrade

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	goversion "github.com/hashicorp/go-version"
	"github.com/schollz/progressbar/v3"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

// Main
// 1) get current binary version
// 2) get the latest version
// 3) compare two version decide needs to download or not
// 4) download newer version zip
// 5) unzip to temp file
// 6) check permission of putting new kubevpn back
// 7) chmod +x, move old to /temp, move new to CURRENT_FOLDER
func Main(current string, client *http.Client) error {
	version, url, err := getManifest(client)
	if err != nil {
		return err
	}
	fmt.Printf("The latest version is: %s\n", version)
	var cVersion, dVersion *goversion.Version
	cVersion, err = goversion.NewVersion(current)
	if err != nil {
		return err
	}
	dVersion, err = goversion.NewVersion(version)
	if err != nil {
		return err
	}
	if cVersion.GreaterThanOrEqual(dVersion) {
		fmt.Println("Already up to date, don't needs to upgrade")
		return nil
	}

	var executable string
	executable, err = os.Executable()
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
		util.RunWithElevated()
		os.Exit(0)
	} else if err != nil {
		return err
	}

	fmt.Printf("Current version is: %s less than latest version: %s, needs to upgrade\n", cVersion, dVersion)

	var temp *os.File
	temp, err = os.CreateTemp("", "")
	if err != nil {
		return err
	}
	err = temp.Close()
	if err != nil {
		return err
	}
	err = download(client, url, temp.Name())
	if err != nil {
		return err
	}
	file, _ := os.CreateTemp("", "")
	err = file.Close()
	if err != nil {
		return err
	}
	err = unzipKubeVPNIntoFile(temp.Name(), file.Name())
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
	err = os.Rename(curFolder, createTemp.Name())
	if err != nil {
		return err
	}
	err = os.Rename(temp.Name(), curFolder)
	return err
}

func getManifest(httpCli *http.Client) (version string, url string, err error) {
	var resp *http.Response
	resp, err = httpCli.Get("https://api.github.com/repos/wencaiwulue/kubevpn/releases/latest")
	if err != nil {
		err = fmt.Errorf("failed to call github api, err: %v", err)
		return
	}
	var all []byte
	all, err = io.ReadAll(resp.Body)
	if err != nil {
		err = fmt.Errorf("failed to read all reponse from github api, err: %v", err)
		return
	}
	var m RootEntity
	err = json.Unmarshal(all, &m)
	if err != nil {
		err = fmt.Errorf("failed to unmarshal reponse, err: %v", err)
		return
	}
	version = m.TagName
	for _, asset := range m.Assets {
		name := fmt.Sprintf("%s-%s-%s", "kubevpn", runtime.GOOS, runtime.GOARCH)
		if runtime.GOOS == "windows" {
			name = fmt.Sprintf("%s-%s-%s.exe", "kubevpn", runtime.GOOS, runtime.GOARCH)
		}
		if name == asset.Name {
			url = asset.BrowserDownloadUrl
			break
		}
	}
	if len(url) == 0 {
		err = fmt.Errorf("can not found latest version url of KubeVPN, resp: %s", string(all))
		return
	}
	return
}

// https://api.github.com/repos/wencaiwulue/kubevpn/releases
// https://github.com/wencaiwulue/kubevpn/releases/download/v1.1.13/kubevpn-windows-arm64.exe
func download(client *http.Client, url string, filename string) error {
	get, err := client.Get(url)
	if err != nil {
		return err
	}
	defer get.Body.Close()
	total := float64(get.ContentLength) / 1024 / 1024
	fmt.Printf("Length: 68276642 (%0.2fM)\n", total)

	var f *os.File
	f, err = os.OpenFile(filename, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer f.Close()
	bar := progressbar.NewOptions(int(get.ContentLength),
		progressbar.OptionSetWriter(os.Stdout),
		progressbar.OptionEnableColorCodes(true),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetWidth(50),
		progressbar.OptionOnCompletion(func() {
			_, _ = fmt.Fprint(os.Stderr, "\n")
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
	buf := make([]byte, 10<<(10*2)) // 10M
	_, err = io.CopyBuffer(io.MultiWriter(f, bar), get.Body, buf)
	return err
}

func unzipKubeVPNIntoFile(zipFile, filename string) error {
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
		return fmt.Errorf("can not found kubevpn")
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
