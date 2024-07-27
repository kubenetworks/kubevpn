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

	"github.com/pkg/errors"
	"github.com/schollz/progressbar/v3"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
)

var (
	address = []string{
		"https://api.github.com/repos/kubenetworks/kubevpn/releases/latest",
		"https://api.github.com/repos/wencaiwulue/kubevpn/releases/latest",
	}
)

const (
	addr = "https://github.com/wencaiwulue/kubevpn/releases/latest"
)

func GetManifest(httpCli *http.Client, os string, arch string) (version string, url string, err error) {
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
		err = errors.Wrap(utilerrors.NewAggregate(errs), "failed to call github api")
		return
	}

	var all []byte
	if all, err = io.ReadAll(resp.Body); err != nil {
		err = errors.Wrap(err, "failed to read all response from github api")
		return
	}
	var m RootEntity
	if err = json.Unmarshal(all, &m); err != nil {
		err = fmt.Errorf("failed to unmarshal response, err: %v", err)
		return
	}
	version = m.TagName
	for _, asset := range m.Assets {
		if strings.Contains(asset.Name, arch) && strings.Contains(asset.Name, os) {
			url = asset.BrowserDownloadUrl
			return
		}
	}

	// if os is not windows and darwin, default is linux
	if !sets.New[string]("windows", "darwin").Has(strings.ToLower(os)) {
		for _, asset := range m.Assets {
			if strings.Contains(asset.Name, "linux") && strings.Contains(asset.Name, arch) {
				url = asset.BrowserDownloadUrl
				return
			}
		}
	}

	err = fmt.Errorf("can not found latest version url of KubeVPN, you can download it manually: %s", addr)
	return
}

// Download
// https://api.github.com/repos/kubenetworks/kubevpn/releases
// https://github.com/kubenetworks/kubevpn/releases/download/v1.1.13/kubevpn-windows-arm64.exe
func Download(client *http.Client, url string, filename string, stdout, stderr io.Writer) error {
	get, err := client.Get(url)
	if err != nil {
		return err
	}
	defer get.Body.Close()
	total := float64(get.ContentLength) / 1024 / 1024
	fmt.Fprintf(stdout, "Length: %d (%0.2fM)\n", get.ContentLength, total)

	var f *os.File
	f, err = os.OpenFile(filename, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0755)
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
	buf := make([]byte, 10<<(10*2)) // 10M
	_, err = io.CopyBuffer(io.MultiWriter(f, bar), get.Body, buf)
	return err
}

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

type RootEntity struct {
	Url             string          `json:"url"`
	AssetsUrl       string          `json:"assets_url"`
	UploadUrl       string          `json:"upload_url"`
	HtmlUrl         string          `json:"html_url"`
	Id              int64           `json:"id"`
	NodeId          string          `json:"node_id"`
	TagName         string          `json:"tag_name"`
	TargetCommitish string          `json:"target_commitish"`
	Name            string          `json:"name"`
	Draft           bool            `json:"draft"`
	Prerelease      bool            `json:"prerelease"`
	CreatedAt       string          `json:"created_at"`
	PublishedAt     string          `json:"published_at"`
	Assets          []AssetsEntity  `json:"assets"`
	TarballUrl      string          `json:"tarball_url"`
	ZipballUrl      string          `json:"zipball_url"`
	Body            string          `json:"body"`
	Reactions       ReactionsEntity `json:"reactions"`
}

type AuthorEntity struct {
	Login             string `json:"login"`
	Id                int64  `json:"id"`
	NodeId            string `json:"node_id"`
	AvatarUrl         string `json:"avatar_url"`
	GravatarId        string `json:"gravatar_id"`
	Url               string `json:"url"`
	HtmlUrl           string `json:"html_url"`
	FollowersUrl      string `json:"followers_url"`
	FollowingUrl      string `json:"following_url"`
	GistsUrl          string `json:"gists_url"`
	StarredUrl        string `json:"starred_url"`
	SubscriptionsUrl  string `json:"subscriptions_url"`
	OrganizationsUrl  string `json:"organizations_url"`
	ReposUrl          string `json:"repos_url"`
	EventsUrl         string `json:"events_url"`
	ReceivedEventsUrl string `json:"received_events_url"`
	Type              string `json:"type"`
	SiteAdmin         bool   `json:"site_admin"`
}

type AssetsEntity struct {
	Url                string         `json:"url"`
	Id                 int64          `json:"id"`
	NodeId             string         `json:"node_id"`
	Name               string         `json:"name"`
	Label              string         `json:"label"`
	Uploader           UploaderEntity `json:"uploader"`
	ContentType        string         `json:"content_type"`
	State              string         `json:"state"`
	Size               int64          `json:"size"`
	DownloadCount      int64          `json:"download_count"`
	CreatedAt          string         `json:"created_at"`
	UpdatedAt          string         `json:"updated_at"`
	BrowserDownloadUrl string         `json:"browser_download_url"`
}

type UploaderEntity struct {
	Login             string `json:"login"`
	Id                int64  `json:"id"`
	NodeId            string `json:"node_id"`
	AvatarUrl         string `json:"avatar_url"`
	GravatarId        string `json:"gravatar_id"`
	Url               string `json:"url"`
	HtmlUrl           string `json:"html_url"`
	FollowersUrl      string `json:"followers_url"`
	FollowingUrl      string `json:"following_url"`
	GistsUrl          string `json:"gists_url"`
	StarredUrl        string `json:"starred_url"`
	SubscriptionsUrl  string `json:"subscriptions_url"`
	OrganizationsUrl  string `json:"organizations_url"`
	ReposUrl          string `json:"repos_url"`
	EventsUrl         string `json:"events_url"`
	ReceivedEventsUrl string `json:"received_events_url"`
	Type              string `json:"type"`
	SiteAdmin         bool   `json:"site_admin"`
}

type ReactionsEntity struct {
	Url        string `json:"url"`
	TotalCount int64  `json:"total_count"`
	Normal1    int64  `json:"+1"`
	Normal11   int64  `json:"-1"`
	Laugh      int64  `json:"laugh"`
	Hooray     int64  `json:"hooray"`
	Confused   int64  `json:"confused"`
	Heart      int64  `json:"heart"`
	Rocket     int64  `json:"rocket"`
	Eyes       int64  `json:"eyes"`
}
