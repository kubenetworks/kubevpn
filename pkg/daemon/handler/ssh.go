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

// sessionRegistry provides thread-safe access to SSH sessions and their readiness signals.
var sessionRegistry = &registry{
	sessions: sync.Map{},
	ready:    sync.Map{},
}

type registry struct {
	sessions sync.Map // map[string]*ssh.Session
	ready    sync.Map // map[string]context.Context
}

func (r *registry) storeSession(id string, session *ssh.Session) {
	r.sessions.Store(id, session)
}

func (r *registry) loadSession(id string) (*ssh.Session, bool) {
	v, ok := r.sessions.Load(id)
	if !ok {
		return nil, false
	}
	return v.(*ssh.Session), true
}

func (r *registry) storeReady(id string, ctx context.Context) {
	r.ready.Store(id, ctx)
}

func (r *registry) loadReady(id string) (context.Context, bool) {
	v, ok := r.ready.Load(id)
	if !ok {
		return nil, false
	}
	return v.(context.Context), true
}

func (r *registry) cleanup(id string) {
	r.sessions.Delete(id)
	r.ready.Delete(id)
}

type wsHandler struct {
	ctx       context.Context
	conn      *websocket.Conn
	sshConfig *pkgssh.SshConfig
	cidr      []string
	width     int
	height    int
	sessionId string
	platform  specs.Platform
	condReady context.CancelFunc
}

// handle orchestrates the SSH workflow:
// 1) optionally install kubevpn and create tunnel
// 2) open an interactive terminal session
func (w *wsHandler) handle(lite bool) {
	ctx, cancel := context.WithCancel(w.ctx)
	defer cancel()

	cli, err := pkgssh.DialSshRemote(ctx, w.sshConfig, ctx.Done())
	if err != nil {
		w.log("Dial ssh remote error: %v", err)
		return
	}
	defer cli.Close()

	if !lite {
		if err := w.createTunnel(ctx, cli); err != nil {
			return
		}
	}

	rw := newConnWatcher(w.conn)
	go func() {
		<-rw.closed()
		cancel()
	}()

	if err := w.terminal(ctx, cli, rw); err != nil {
		w.log("Enter terminal error: %v", err)
	}
}

func (w *wsHandler) createTunnel(ctx context.Context, cli *ssh.Client) error {
	if err := w.installKubevpnOnRemote(ctx, cli); err != nil {
		return err
	}

	clientIP, err := util.GetLocalIPNet()
	if err != nil {
		w.log("Get client IP error: %v", err)
		return err
	}

	localPort, err := util.GetAvailableTCPPort()
	if err != nil {
		return err
	}
	remote, _ := netip.ParseAddrPort(net.JoinHostPort("127.0.0.1", "10801"))
	local, _ := netip.ParseAddrPort(net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort)))

	if err := pkgssh.PortMapUntil(ctx, w.sshConfig, remote, local); err != nil {
		w.log("Port map error: %v", err)
		return err
	}

	serverIP, stderr, err := pkgssh.RemoteRun(cli, fmt.Sprintf("kubevpn ssh-daemon --client-ip %s", clientIP.String()), nil)
	if err != nil {
		plog.G(ctx).Errorf("Failed to run remote command: %v, stdout: %s, stderr: %s", err, string(serverIP), string(stderr))
		w.log("Start kubevpn server error: %v", err)
		return err
	}
	ip, _, err := net.ParseCIDR(string(serverIP))
	if err != nil {
		w.log("Failed to parse server IP %s, stderr: %s: %v", string(serverIP), string(stderr), err)
		return err
	}
	w.log(util.FormatBanner(fmt.Sprintf("You can use client: %s to communicate with server: %s", clientIP.IP.String(), ip.String())))
	w.cidr = append(w.cidr, string(serverIP))

	nodes := []*core.Node{
		core.NewNode("tun", "").
			WithForward(fmt.Sprintf("tcp://127.0.0.1:%d", localPort)).
			WithParam("net", clientIP.String()).
			WithParam("route", strings.Join(w.cidr, ",")),
	}
	servers, err := core.GenerateServersFromNodes(nodes, nil)
	if err != nil {
		w.log("Failed to parse route: %v", err)
		return err
	}
	go func() {
		if err := handler.Run(ctx, servers); err != nil {
			plog.G(ctx).Errorf("TUN server exited: %v", err)
		}
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

// connWatcher wraps a net.Conn and signals when the connection is broken.
type connWatcher struct {
	ch chan struct{}
	sync.Once
	net.Conn
}

func newConnWatcher(conn net.Conn) *connWatcher {
	return &connWatcher{ch: make(chan struct{}), Conn: conn}
}

func (cw *connWatcher) Read(b []byte) (int, error) {
	n, err := cw.Conn.Read(b)
	if err != nil {
		cw.Do(func() { close(cw.ch) })
	}
	return n, err
}

func (cw *connWatcher) Write(p []byte) (int, error) {
	n, err := cw.Conn.Write(p)
	if err != nil {
		cw.Do(func() { close(cw.ch) })
	}
	return n, err
}

func (cw *connWatcher) closed() <-chan struct{} {
	return cw.ch
}

func (w *wsHandler) terminal(ctx context.Context, cli *ssh.Client, conn io.ReadWriter) error {
	session, err := cli.NewSession()
	if err != nil {
		w.log("New session error: %v", err)
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

	sessionRegistry.storeSession(w.sessionId, session)
	w.condReady()

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.ECHOCTL:       0,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err = session.RequestPty("xterm-256color", w.height, w.width, modes); err != nil {
		w.log("Request pty error: %v", err)
		return err
	}
	if err = session.Shell(); err != nil {
		w.log("Start shell error: %v", err)
		return err
	}
	w.log(SshTerminalReadyFormat, w.sessionId)
	return session.Wait()
}

func (w *wsHandler) installKubevpnOnRemote(ctx context.Context, sshClient *ssh.Client) (err error) {
	defer func() {
		if err == nil {
			w.log("Remote daemon server version: %s", startDaemonProcess(sshClient))
		}
	}()

	if _, _, e := pkgssh.RemoteRun(sshClient, "kubevpn version", nil); e == nil {
		w.log("Found command kubevpn command on remote")
		return nil
	}

	plog.G(ctx).Info("Install command kubevpn...")
	w.log("Install kubevpn on remote server...")

	client := http.DefaultClient
	if config.GitHubOAuthToken != "" {
		client = oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: config.GitHubOAuthToken, TokenType: "Bearer"}))
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

	if err = util.UnzipKubeVPNIntoFile(temp.Name(), tempBin.Name()); err != nil {
		return err
	}
	if err = os.Chmod(tempBin.Name(), 0755); err != nil {
		return err
	}

	w.log("Scp kubevpn to remote server ~/.kubevpn/kubevpn")
	return pkgssh.SCPAndExec(ctx, w.conn, w.conn, sshClient, tempBin.Name(), "kubevpn",
		"chmod +x ~/.kubevpn/kubevpn",
		"sudo mv ~/.kubevpn/kubevpn /usr/local/bin/kubevpn",
	)
}

func (w *wsHandler) log(format string, a ...any) {
	str := format
	if len(a) != 0 {
		str = fmt.Sprintf(format, a...)
	}
	w.conn.Write([]byte(str + "\r\n"))
	plog.G(w.ctx).Infof(format, a...)
}

// Ssh is the configuration received from the websocket client.
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
		if err := json.Unmarshal([]byte(b), &conf); err != nil {
			conn.Write([]byte(err.Error()))
			conn.Close()
			return
		}
		defer sessionRegistry.cleanup(conf.SessionID)

		ctx, cancelFunc := context.WithCancel(conn.Request().Context())
		defer cancelFunc()

		h := &wsHandler{
			ctx:       ctx,
			sshConfig: &conf.Config,
			conn:      conn,
			cidr:      conf.ExtraCIDR,
			width:     conf.Width,
			height:    conf.Height,
			sessionId: conf.SessionID,
			platform:  platforms.MustParse(conf.Platform),
			condReady: cancelFunc,
		}
		sessionRegistry.storeReady(conf.SessionID, ctx)
		defer conn.Close()
		h.handle(conf.Lite)
	}))

	http.Handle("/resize", websocket.Handler(func(conn *websocket.Conn) {
		sessionID := conn.Request().Header.Get("session-id")
		plog.G(context.Background()).Infof("Resize: %s", sessionID)
		defer conn.Close()

		readyCtx, ok := sessionRegistry.loadReady(sessionID)
		if !ok {
			return
		}

		select {
		case <-conn.Request().Context().Done():
			return
		case <-readyCtx.Done():
		}

		session, ok := sessionRegistry.loadSession(sessionID)
		if !ok {
			return
		}

		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadString('\n')
			if errors.Is(err, io.EOF) {
				return
			} else if err != nil {
				plog.G(context.Background()).Errorf("Failed to read resize event for session %s: %v", sessionID, err)
				return
			}
			var size remotecommand.TerminalSize
			if err = json.Unmarshal([]byte(line), &size); err != nil {
				plog.G(context.Background()).Errorf("Unmarshal terminal size failed: %v", err)
				continue
			}
			if err = session.WindowChange(int(size.Height), int(size.Width)); err != nil {
				if errors.Is(err, io.EOF) {
					return
				}
				plog.G(context.Background()).Errorf("Session %s window change failed: %v", sessionID, err)
			}
		}
	}))
}
