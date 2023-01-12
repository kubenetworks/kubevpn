package upgrade

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"

	goversion "github.com/hashicorp/go-version"
	"k8s.io/utils/pointer"
)

// Main
// 1) get current binary version
// 2) get the latest version
// 3) compare two version decide needs to download or not
// 4) download newer version
// 5) chmod +x, move old to /temp, move new to CURRENT_FOLDER
func Main(current string, client *http.Client) error {
	version, url, err := getManifest(client)
	if err != nil {
		return err
	}
	cVersion, err := goversion.NewVersion(current)
	if err != nil {
		return err
	}
	dVersion, err := goversion.NewVersion(version)
	if err != nil {
		return err
	}
	if cVersion.GreaterThanOrEqual(dVersion) {
		fmt.Println("current version is bigger than latest version, don't needs to download")
		return nil
	}
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
	err = os.Chmod(temp.Name(), 0755)
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
		err = fmt.Errorf("failed to resp latest version of KubeVPN, err: %v", err)
		return
	}
	all, err := io.ReadAll(resp.Body)
	if err != nil {
		err = fmt.Errorf("failed to resp latest version of KubeVPN, err: %v", err)
		return
	}
	var m RootEntity
	err = json.Unmarshal(all, &m)
	if err != nil {
		err = fmt.Errorf("failed to resp latest version of KubeVPN, err: %v", err)
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
		err = fmt.Errorf("failed to resp latest version url of KubeVPN, resp: %s", string(all))
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
	pr := make(chan Update, 1)
	p := &progressReader{
		rc:    get.Body,
		count: pointer.Int64(0),
		progress: &progress{
			updates:    pr,
			lastUpdate: &Update{},
		},
	}
	p.progress.total(get.ContentLength)
	go func() {
		var last string
		for {
			select {
			case u, ok := <-pr:
				if ok {
					per := float32(u.Complete) / float32(u.Total) * 100
					s := fmt.Sprintf("%.0f%%", per)
					if last != s {
						fmt.Printf(fmt.Sprintf("\r%s%%", s))
						last = s
					}
				}
			}
		}
	}()
	var f *os.File
	f, err = os.OpenFile(filename, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 10<<(10*2)) // 10M
	_, err = io.CopyBuffer(f, p, buf)
	return err
}
