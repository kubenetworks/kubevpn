package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/oauth2"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	pkgssh "github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (w *wsHandler) installKubevpnOnRemote(ctx context.Context, sshClient *ssh.Client) (err error) {
	defer func() {
		if err == nil {
			w.log("Remote daemon server version: %s", startDaemonProcess(sshClient))
		}
	}()

	if _, _, e := pkgssh.RemoteRun(sshClient, "kubevpn version", nil); e == nil {
		w.log("Found kubevpn command on remote")
		return nil
	}

	plog.G(ctx).Info("Install command kubevpn...")
	w.log("Install kubevpn on remote server...")

	const httpClientTimeout = 30 * time.Second
	client := &http.Client{Timeout: httpClientTimeout}
	if config.GitHubOAuthToken != "" {
		client = oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: config.GitHubOAuthToken, TokenType: "Bearer"}))
		// oauth2.NewClient drops the Timeout; re-apply it so the request cannot hang.
		client.Timeout = httpClientTimeout
	}
	latestVersion, url, err := util.GetManifest(client, w.platform.OS, w.platform.Architecture)
	if err != nil {
		w.log("Get latest kubevpn version failed: %v", err)
		return err
	}
	w.log("The latest version is %s", latestVersion)

	temp, err := os.CreateTemp("", "")
	if err != nil {
		return err
	}
	temp.Close()
	defer os.Remove(temp.Name())

	w.log("Downloading kubevpn...")
	if err = util.Download(client, url, temp.Name(), w.conn, w.conn); err != nil {
		return err
	}

	tempBin, err := os.CreateTemp("", "kubevpn")
	if err != nil {
		return err
	}
	tempBin.Close()
	defer os.Remove(tempBin.Name())

	if err = util.UnzipKubeVPNIntoFile(temp.Name(), tempBin.Name()); err != nil {
		return err
	}
	if err = os.Chmod(tempBin.Name(), config.FileModeExecutable); err != nil {
		return err
	}

	w.log("Scp kubevpn to remote server ~/.kubevpn/kubevpn")
	return pkgssh.SCPAndExec(ctx, w.conn, w.conn, sshClient, tempBin.Name(), "kubevpn",
		"chmod +x ~/.kubevpn/kubevpn",
		"sudo mv ~/.kubevpn/kubevpn /usr/local/bin/kubevpn",
	)
}

func startDaemonProcess(cli *ssh.Client) string {
	_, _, _ = pkgssh.RemoteRun(cli, "kubevpn status > /dev/null 2>&1 &", nil)
	output, _, err := pkgssh.RemoteRun(cli, "kubevpn version", nil)
	if err != nil {
		return ""
	}
	return parseDaemonVersion(output)
}

func parseDaemonVersion(output []byte) string {
	type versionData struct {
		DaemonVersion string `json:"Daemon"`
	}
	buf := bufio.NewReader(bytes.NewReader(output))
	_, _, _ = buf.ReadLine()
	rest, err := io.ReadAll(buf)
	if err != nil {
		return ""
	}
	jsonBytes, err := yaml.YAMLToJSON(rest)
	if err != nil {
		return ""
	}
	var data versionData
	if json.Unmarshal(jsonBytes, &data) != nil {
		return ""
	}
	return data.DaemonVersion
}
