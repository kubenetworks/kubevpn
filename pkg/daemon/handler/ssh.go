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
	"sync"
	"time"

	"github.com/containerd/containerd/platforms"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/websocket"
	"golang.org/x/oauth2"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	pkgssh "github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

const SshTerminalReadyFormat = "Enter terminal %s"

type wsHandler struct {
	conn      *websocket.Conn
	sshConfig *pkgssh.SshConfig
	cidr      []string
	width     int
	height    int
	sessionId string
	platform  specs.Platform
	condReady context.CancelFunc
}

// handle
// 0) remote server install kubevpn if not found
// 1) start remote kubevpn server
// 2) start local tunnel
// 3) ssh terminal
func (w *wsHandler) handle(c context.Context, lite bool) {
	ctx, f := context.WithCancel(c)
	defer f()

	cli, err := pkgssh.DialSshRemote(ctx, w.sshConfig, ctx.Done())
	if err != nil {
		w.Log("Dial ssh remote error: %v", err)
		return
	}
	defer cli.Close()

	if !lite {
		err = w.createTunnel(ctx, cli)
		if err != nil {
			return
		}
	}
	rw := NewReadWriteWrapper(w.conn)
	go func() {
		<-rw.IsClosed()
		f()
	}()
	err = w.terminal(ctx, cli, rw)
	if err != nil {
		w.Log("Enter terminal error: %v", err)
	}
	return
}

func (w *wsHandler) createTunnel(ctx context.Context, cli *ssh.Client) error {
	err := w.installKubevpnOnRemote(ctx, cli)
	if err != nil {
		//w.Log("Install kubevpn error: %v", err)
		return err
	}

	clientIP, err := util.GetIPBaseNic()
	if err != nil {
		w.Log("Get client IP error: %v", err)
		return err
	}

	remotePort := 10801
	var localPort int
	localPort, err = util.GetAvailableTCPPortOrDie()
	if err != nil {
		return err
	}
	var remote netip.AddrPort
	remote, err = netip.ParseAddrPort(net.JoinHostPort("127.0.0.1", strconv.Itoa(remotePort)))
	if err != nil {
		return err
	}
	var local netip.AddrPort
	local, err = netip.ParseAddrPort(net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort)))
	if err != nil {
		return err
	}
	err = pkgssh.PortMapUntil(ctx, w.sshConfig, remote, local)
	if err != nil {
		w.Log("Port map error: %v", err)
		return err
	}
	cmd := fmt.Sprintf(`kubevpn ssh-daemon --client-ip %s`, clientIP.String())
	serverIP, stderr, err := pkgssh.RemoteRun(cli, cmd, nil)
	if err != nil {
		plog.G(ctx).Errorf("Failed to run remote command: %v, stdout: %s, stderr: %s", err, string(serverIP), string(stderr))
		w.Log("Start kubevpn server error: %v", err)
		return err
	}
	ip, _, err := net.ParseCIDR(string(serverIP))
	if err != nil {
		w.Log("Failed to parse server IP %s, stderr: %s: %v", string(serverIP), string(stderr), err)
		return err
	}
	msg := util.PrintStr(fmt.Sprintf("You can use client: %s to communicate with server: %s", clientIP.IP.String(), ip.String()))
	w.Log(msg)
	w.cidr = append(w.cidr, string(serverIP))
	r := core.Route{
		Listeners: []string{
			fmt.Sprintf("tun:/%s?net=%s&route=%s", fmt.Sprintf("tcp://127.0.0.1:%d", localPort), clientIP, strings.Join(w.cidr, ",")),
		},
		Retries: 5,
	}
	servers, err := handler.Parse(r)
	if err != nil {
		plog.G(ctx).Errorf("Failed to parse route: %v", err)
		w.Log("Failed to parse route: %v", err)
		return err
	}
	go func() {
		err := handler.Run(ctx, servers)
		plog.G(ctx).Errorf("Failed to run: %v", err)
		w.Log("Failed to run: %v", err)
	}()
	plog.G(ctx).Info("Connected private safe tunnel")
	go func() {
		for ctx.Err() == nil {
			_, _ = util.Ping(ctx, clientIP.IP.String(), ip.String())
			time.Sleep(time.Second * 15)
		}
	}()
	return nil
}

// startup daemon process if daemon process not start
func startDaemonProcess(cli *ssh.Client) string {
	startDaemonCmd := fmt.Sprintf(`kubevpn status > /dev/null 2>&1 &`)
	_, _, _ = pkgssh.RemoteRun(cli, startDaemonCmd, nil)
	output, _, err := pkgssh.RemoteRun(cli, "kubevpn version", nil)
	if err != nil {
		return ""
	}
	version := getDaemonVersionFromOutput(output)
	return version
}

func getDaemonVersionFromOutput(output []byte) (version string) {
	type Data struct {
		DaemonVersion string `json:"Daemon"`
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

type ReadWriteWrapper struct {
	closed chan any
	sync.Once
	net.Conn
}

func NewReadWriteWrapper(conn net.Conn) *ReadWriteWrapper {
	return &ReadWriteWrapper{
		closed: make(chan any),
		Once:   sync.Once{},
		Conn:   conn,
	}
}

func (rw *ReadWriteWrapper) Read(b []byte) (int, error) {
	n, err := rw.Conn.Read(b)
	if err != nil {
		rw.Do(func() {
			close(rw.closed)
		})
	}
	return n, err
}

func (rw *ReadWriteWrapper) Write(p []byte) (int, error) {
	n, err := rw.Conn.Write(p)
	if err != nil {
		rw.Do(func() {
			close(rw.closed)
		})
	}
	return n, err
}

func (rw *ReadWriteWrapper) IsClosed() chan any {
	return rw.closed
}

func (w *wsHandler) terminal(ctx context.Context, cli *ssh.Client, conn io.ReadWriter) error {
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
	if err = session.RequestPty("xterm-256color", height, width, modes); err != nil {
		w.Log("Request pty error: %v", err)
		return err
	}
	if err = session.Shell(); err != nil {
		w.Log("Start shell error: %v", err)
		return err
	}
	w.Log(SshTerminalReadyFormat, w.sessionId)
	return session.Wait()
}

func (w *wsHandler) installKubevpnOnRemote(ctx context.Context, sshClient *ssh.Client) (err error) {
	defer func() {
		if err == nil {
			w.Log("Remote daemon server version: %s", startDaemonProcess(sshClient))
		}
	}()

	cmd := "kubevpn version"
	_, _, err = pkgssh.RemoteRun(sshClient, cmd, nil)
	if err == nil {
		w.Log("Found command kubevpn command on remote")
		return nil
	}
	plog.G(ctx).Infof("Install command kubevpn...")
	w.Log("Install kubevpn on remote server...")
	var client = http.DefaultClient
	if config.GitHubOAuthToken != "" {
		client = oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: config.GitHubOAuthToken, TokenType: "Bearer"}))
	}
	latestVersion, url, err := util.GetManifest(client, w.platform.OS, w.platform.Architecture)
	if err != nil {
		w.Log("Get latest kubevpn version failed: %v", err)
		return err
	}
	w.Log("The latest version is %s", latestVersion)
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
	plog.G(ctx).Infof("Upgrade daemon...")
	w.Log("Scp kubevpn to remote server ~/.kubevpn/kubevpn")
	cmds := []string{
		"chmod +x ~/.kubevpn/kubevpn",
		"sudo mv ~/.kubevpn/kubevpn /usr/local/bin/kubevpn",
	}
	err = pkgssh.SCPAndExec(ctx, w.conn, w.conn, sshClient, tempBin.Name(), "kubevpn", cmds...)
	return err
}

func (w *wsHandler) Log(format string, a ...any) {
	str := format
	if len(a) != 0 {
		str = fmt.Sprintf(format, a...)
	}
	w.conn.Write([]byte(str + "\r\n"))
	plog.G(context.Background()).Infof(format, a...)
}

var SessionMap = make(map[string]*ssh.Session)
var CondReady = make(map[string]context.Context)

type Ssh struct {
	Config    pkgssh.SshConfig
	ExtraCIDR []string
	Width     int
	Height    int
	Platform  string
	SessionID string
	Lite      bool
}

func init() {
	http.Handle("/ws", websocket.Handler(func(conn *websocket.Conn) {
		b := conn.Request().Header.Get("ssh")
		var conf Ssh
		err := json.Unmarshal([]byte(b), &conf)
		if err != nil {
			_, _ = conn.Write([]byte(err.Error()))
			_ = conn.Close()
			return
		}
		defer delete(SessionMap, conf.SessionID)
		defer delete(CondReady, conf.SessionID)

		ctx, cancelFunc := context.WithCancel(conn.Request().Context())
		h := &wsHandler{
			sshConfig: &conf.Config,
			conn:      conn,
			cidr:      conf.ExtraCIDR,
			width:     conf.Width,
			height:    conf.Height,
			sessionId: conf.SessionID,
			platform:  platforms.MustParse(conf.Platform),
			condReady: cancelFunc,
		}
		CondReady[conf.SessionID] = ctx
		defer conn.Close()
		h.handle(conn.Request().Context(), conf.Lite)
	}))
	http.Handle("/resize", websocket.Handler(func(conn *websocket.Conn) {
		sessionID := conn.Request().Header.Get("session-id")
		plog.G(context.Background()).Infof("Resize: %s", sessionID)

		defer conn.Close()

		if CondReady[sessionID] == nil {
			return
		}

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
				plog.G(context.Background()).Errorf("Failed to read session %s window resize event: %v", sessionID, err)
				return
			}
			var r remotecommand.TerminalSize
			err = json.Unmarshal([]byte(readString), &r)
			if err != nil {
				plog.G(context.Background()).Errorf("Unmarshal into terminal size failed: %v", err)
				continue
			}
			plog.G(context.Background()).Debugf("Session %s change termianl size to w: %d h:%d", sessionID, r.Width, r.Height)
			err = session.WindowChange(int(r.Height), int(r.Width))
			if errors.Is(err, io.EOF) {
				return
			} else if err != nil {
				plog.G(context.Background()).Errorf("Session %s windows change w: %d h: %d failed: %v", sessionID, r.Width, r.Height, err)
			}
		}
	}))
}
