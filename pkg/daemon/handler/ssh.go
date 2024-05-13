package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/websocket"
	"golang.org/x/oauth2"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type wsHandler struct {
	conn      *websocket.Conn
	sshConfig *util.SshConfig
	cidr      []string
	width     int
	height    int
	sessionId string
	condReady context.CancelFunc
}

// handle
// 0) remote server install kubevpn if not found
// 1) start remote kubevpn server
// 2) start local tunnel
// 3) ssh terminal
func (w *wsHandler) handle(ctx context.Context) {
	ctx, f := context.WithCancel(ctx)
	defer f()

	cli, err := util.DialSshRemote(ctx, w.sshConfig)
	if err != nil {
		w.Log("Dial ssh remote error: %v", err)
		return
	}
	defer cli.Close()

	err = w.installKubevpnOnRemote(ctx, cli)
	if err != nil {
		w.Log("Install kubevpn error: %v", err)
		return
	}

	clientIP, err := util.GetIPBaseNic()
	if err != nil {
		w.Log("Get client ip error: %v", err)
		return
	}

	remotePort := 10800
	var localPort int
	localPort, err = util.GetAvailableTCPPortOrDie()
	if err != nil {
		return
	}
	var remote netip.AddrPort
	remote, err = netip.ParseAddrPort(net.JoinHostPort("127.0.0.1", strconv.Itoa(remotePort)))
	if err != nil {
		return
	}
	var local netip.AddrPort
	local, err = netip.ParseAddrPort(net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort)))
	if err != nil {
		return
	}
	err = util.PortMapUntil(ctx, w.sshConfig, remote, local)
	if err != nil {
		w.Log("Port map error: %v", err)
		return
	}
	cmd := fmt.Sprintf(`export %s=%s && kubevpn ssh-daemon --client-ip %s`, config.EnvStartSudoKubeVPNByKubeVPN, "true", clientIP.String())
	serverIP, stderr, err := util.RemoteRun(cli, cmd, nil)
	if err != nil {
		log.Errorf("run error: %v", err)
		log.Errorf("run stdout: %v", string(serverIP))
		log.Errorf("run stderr: %v", string(stderr))
		w.Log("Start kubevpn server error: %v", err)
		return
	}
	ip, _, err := net.ParseCIDR(string(serverIP))
	if err != nil {
		w.Log("Parse server ip %s, stderr: %s: %v", string(serverIP), string(stderr), err)
		return
	}
	msg := fmt.Sprintf("| You can use client: %s to communicate with server: %s |", clientIP.IP.String(), ip.String())
	w.PrintLine(msg)
	w.cidr = append(w.cidr, string(serverIP))
	r := core.Route{
		ServeNodes: []string{
			fmt.Sprintf("tun:/127.0.0.1:8422?net=%s&route=%s", clientIP, strings.Join(w.cidr, ",")),
		},
		ChainNode: fmt.Sprintf("tcp://127.0.0.1:%d", localPort),
		Retries:   5,
	}
	servers, err := handler.Parse(r)
	if err != nil {
		log.Errorf("parse route error: %v", err)
		w.Log("Parse route error: %v", err)
		return
	}
	go func() {
		err := handler.Run(ctx, servers)
		log.Errorf("Run error: %v", err)
		w.Log("Run error: %v", err)
	}()
	log.Info("tunnel connected")
	go func() {
		for ctx.Err() == nil {
			_, _ = util.Ping(ctx, clientIP.IP.String(), ip.String())
			time.Sleep(time.Second * 5)
		}
	}()
	err = w.terminal(ctx, cli, w.conn)
	if err != nil {
		w.Log("Enter terminal error: %v", err)
	}
	return
}

// startup daemon process if daemon process not start
func startDaemonProcess(cli *ssh.Client) {
	startDaemonCmd := fmt.Sprintf(`export %s=%s && kubevpn status > /dev/null 2>&1 &`, config.EnvStartSudoKubeVPNByKubeVPN, "true")
	_, _, _ = util.RemoteRun(cli, startDaemonCmd, nil)
	ticker := time.NewTicker(time.Millisecond * 50)
	defer ticker.Stop()
	for range ticker.C {
		output, _, err := util.RemoteRun(cli, "kubevpn version", nil)
		if err != nil {
			continue
		}
		version := getDaemonVersionFromOutput(output)
		if version != "" && version != "unknown" {
			break
		}
	}
}

func getDaemonVersionFromOutput(output []byte) (version string) {
	type Data struct {
		DaemonVersion string `json:"DaemonVersion"`
	}
	// remove first line
	buf := bufio.NewReader(bytes.NewReader(output))
	_, _, _ = buf.ReadLine()
	restBytes, err := io.ReadAll(buf)
	if err != nil {
		return
	}
	jsonBytes, err := yaml.YAMLToJSON(restBytes)
	if err != nil {
		return
	}
	var data Data
	err = json.Unmarshal(jsonBytes, &data)
	if err != nil {
		return
	}
	return data.DaemonVersion
}

func (w *wsHandler) terminal(ctx context.Context, cli *ssh.Client, conn *websocket.Conn) error {
	session, err := cli.NewSession()
	if err != nil {
		w.Log("New session error: %v", err)
		return err
	}
	defer session.Close()
	go func() {
		<-ctx.Done()
		session.Close()
	}()
	session.Stdout = conn
	session.Stderr = conn
	session.Stdin = conn

	SessionMap[w.sessionId] = session
	w.condReady()

	width, height := w.width, w.height
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.ECHOCTL:       0,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm", height, width, modes); err != nil {
		w.Log("Request pty error: %v", err)
		return err
	}
	if err = session.Shell(); err != nil {
		w.Log("Start shell error: %v", err)
		return err
	}
	return session.Wait()
}

func (w *wsHandler) installKubevpnOnRemote(ctx context.Context, sshClient *ssh.Client) (err error) {
	defer func() {
		if err == nil {
			startDaemonProcess(sshClient)
		}
	}()

	cmd := "kubevpn version"
	_, _, err = util.RemoteRun(sshClient, cmd, nil)
	if err == nil {
		w.Log("Found command kubevpn command on remote")
		return nil
	}
	log.Infof("remote kubevpn command not found, try to install it...")
	w.Log("Try to install kubevpn on remote server")
	var client = http.DefaultClient
	if config.GitHubOAuthToken != "" {
		client = oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: config.GitHubOAuthToken, TokenType: "Bearer"}))
	}
	latestVersion, latestCommit, url, err := util.GetManifest(client, "linux", "amd64")
	if err != nil {
		w.Log("Get latest kubevpn version failed: %v", err)
		return err
	}
	w.Log("The latest version is: %s, commit: %s", latestVersion, latestCommit)
	var temp *os.File
	temp, err = os.CreateTemp("", "")
	if err != nil {
		return err
	}
	err = temp.Close()
	if err != nil {
		return err
	}
	w.Log("Downloading kubevpn...")
	err = util.Download(client, url, temp.Name(), w.conn, w.conn)
	if err != nil {
		return err
	}
	var tempBin *os.File
	tempBin, err = os.CreateTemp("", "kubevpn")
	if err != nil {
		return err
	}
	err = tempBin.Close()
	if err != nil {
		return err
	}
	err = util.UnzipKubeVPNIntoFile(temp.Name(), tempBin.Name())
	if err != nil {
		return err
	}
	// scp kubevpn to remote ssh server and run daemon
	err = os.Chmod(tempBin.Name(), 0755)
	if err != nil {
		return err
	}
	err = os.Remove(temp.Name())
	if err != nil {
		return err
	}
	log.Infof("Upgrade daemon...")
	w.Log("Scp kubevpn to remote server ~/.kubevpn/kubevpn")
	cmds := []string{
		"chmod +x ~/.kubevpn/kubevpn",
		"sudo mv ~/.kubevpn/kubevpn /usr/local/bin/kubevpn",
	}
	err = util.SCPAndExec(w.conn, w.conn, sshClient, tempBin.Name(), "kubevpn", cmds...)
	return err
}

func (w *wsHandler) Log(format string, a ...any) {
	str := format
	if len(a) != 0 {
		str = fmt.Sprintf(format, a...)
	}
	w.conn.Write([]byte(str + "\r\n"))
	log.Infof(format, a...)
}

func (w *wsHandler) PrintLine(msg string) {
	line := "+" + strings.Repeat("-", len(msg)-2) + "+"
	w.Log(line)
	w.Log(msg)
	w.Log(line)
}

var SessionMap = make(map[string]*ssh.Session)
var CondReady = make(map[string]context.Context)

func init() {
	http.Handle("/ws", websocket.Handler(func(conn *websocket.Conn) {
		sshConfig := util.SshConfig{
			Addr:             conn.Request().Header.Get("ssh-addr"),
			User:             conn.Request().Header.Get("ssh-username"),
			Password:         conn.Request().Header.Get("ssh-password"),
			Keyfile:          conn.Request().Header.Get("ssh-keyfile"),
			ConfigAlias:      conn.Request().Header.Get("ssh-alias"),
			GSSAPIPassword:   conn.Request().Header.Get("gssapi-password"),
			GSSAPIKeytabConf: conn.Request().Header.Get("gssapi-keytab"),
			GSSAPICacheFile:  conn.Request().Header.Get("gssapi-cache"),
		}
		var extraCIDR []string
		if v := conn.Request().Header.Get("extra-cidr"); v != "" {
			extraCIDR = strings.Split(v, ",")
		}
		width, _ := strconv.Atoi(conn.Request().Header.Get("width"))
		height, _ := strconv.Atoi(conn.Request().Header.Get("height"))
		sessionID := conn.Request().Header.Get("session-id")
		defer delete(SessionMap, sessionID)
		defer delete(CondReady, sessionID)

		ctx, cancelFunc := context.WithCancel(conn.Request().Context())
		h := &wsHandler{
			sshConfig: &sshConfig,
			conn:      conn,
			cidr:      extraCIDR,
			width:     width,
			height:    height,
			sessionId: sessionID,
			condReady: cancelFunc,
		}
		CondReady[sessionID] = ctx
		defer conn.Close()
		h.handle(conn.Request().Context())
	}))
	http.Handle("/resize", websocket.Handler(func(conn *websocket.Conn) {
		sessionID := conn.Request().Header.Get("session-id")
		log.Infof("resize: %s", sessionID)

		defer conn.Close()

		var session *ssh.Session
		select {
		case <-conn.Request().Context().Done():
			return
		case <-CondReady[sessionID].Done():
			session = SessionMap[sessionID]
		}
		if session == nil {
			return
		}

		reader := bufio.NewReader(conn)
		for {
			readString, err := reader.ReadString('\n')
			if errors.Is(err, io.EOF) {
				return
			} else if err != nil {
				log.Errorf("failed to read session %s window resize event: %v", sessionID, err)
				return
			}
			var r remotecommand.TerminalSize
			err = json.Unmarshal([]byte(readString), &r)
			if err != nil {
				log.Errorf("unmarshal into terminal size failed: %v", err)
				continue
			}
			log.Debugf("session %s change termianl size to w: %d h:%d", sessionID, r.Width, r.Height)
			err = session.WindowChange(int(r.Height), int(r.Width))
			if errors.Is(err, io.EOF) {
				return
			} else if err != nil {
				log.Errorf("session %s windos change w: %d h: %d failed: %v", sessionID, r.Width, r.Height, err)
			}
		}
	}))
}
