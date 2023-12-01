package handler

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/websocket"
	"golang.org/x/oauth2"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/core"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/util"
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
func (w *wsHandler) handle(ctx2 context.Context) {
	conn := w.conn
	sshConfig := w.sshConfig
	cidr := w.cidr
	ctx, cancelFunc := context.WithCancel(ctx2)
	defer cancelFunc()

	err := w.remoteInstallKubevpnIfCommandNotFound(ctx, sshConfig)
	if err != nil {
		w.Log("Install kubevpn error: %v", err)
		return
	}

	clientIP, err := util.GetIPBaseNic()
	if err != nil {
		w.Log("Get client ip error: %v", err)
		return
	}

	local, err := w.portMap(ctx, sshConfig)
	if err != nil {
		w.Log("Port map error: %v", err)
		return
	}
	cmd := fmt.Sprintf(`export %s=%s && kubevpn ssh-daemon --client-ip %s`, config.EnvStartSudoKubeVPNByKubeVPN, "true", clientIP.String())
	serverIP, stderr, err := util.RemoteRun(sshConfig, cmd, nil)
	if err != nil {
		log.Errorf("run error: %v", err)
		log.Errorf("run stdout: %v", string(serverIP))
		log.Errorf("run stderr: %v", string(stderr))
		w.Log("Start kubevpn server error: %v", err)
		return
	}
	ip, _, err := net.ParseCIDR(string(serverIP))
	if err != nil {
		w.Log("Parse server ip error: %v", err)
		return
	}
	msg := fmt.Sprintf("| You can use client: %s to communicate with server: %s |", clientIP.IP.String(), ip.String())
	w.PrintLine(msg)
	cidr = append(cidr, string(serverIP))
	r := core.Route{
		ServeNodes: []string{
			fmt.Sprintf("tun:/127.0.0.1:8422?net=%s&route=%s", clientIP, strings.Join(cidr, ",")),
		},
		ChainNode: fmt.Sprintf("tcp://127.0.0.1:%d", local),
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
	tun, err := util.GetTunDevice(clientIP.IP)
	if err != nil {
		w.Log("Get tun device error: %v", err)
		return
	}
	log.Info("tunnel connected")
	go func() {
		ticker := time.NewTicker(time.Second * 2)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = util.Ping(clientIP.IP.String())
				_, _ = util.Ping(ip.String())
				_ = exec.CommandContext(ctx, "ping", "-c", "4", "-b", tun.Name, ip.String()).Run()
			}
		}
	}()
	err = w.terminal(ctx, sshConfig, conn)
	if err != nil {
		w.Log("Enter terminal error: %v", err)
	}
	return
}

func (w *wsHandler) portMap(ctx context.Context, conf *util.SshConfig) (localPort int, err error) {
	removePort := 10800
	localPort, err = util.GetAvailableTCPPortOrDie()
	if err != nil {
		return
	}
	var remote netip.AddrPort
	remote, err = netip.ParseAddrPort(net.JoinHostPort("127.0.0.1", strconv.Itoa(removePort)))
	if err != nil {
		return
	}
	var local netip.AddrPort
	local, err = netip.ParseAddrPort(net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort)))
	if err != nil {
		return
	}

	// pre-check network ip connect
	var cli *ssh.Client
	cli, err = util.DialSshRemote(conf)
	if err != nil {
		return
	} else {
		_ = cli.Close()
	}
	errChan := make(chan error, 1)
	readyChan := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			err := util.Main(ctx, remote, local, conf, readyChan)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					log.Errorf("ssh forward failed err: %v", err)
					w.Log("Ssh forward failed err: %v", err)
				}
				select {
				case errChan <- err:
				default:
				}
			}
		}
	}()
	select {
	case <-readyChan:
		return
	case err = <-errChan:
		w.Log("Ssh forward failed err: %v", err)
		log.Errorf("ssh proxy err: %v", err)
		return
	}
}

func (w *wsHandler) terminal(ctx context.Context, conf *util.SshConfig, conn *websocket.Conn) error {
	cli, err := util.DialSshRemote(conf)
	if err != nil {
		w.Log("Dial remote error: %v", err)
		return err
	}
	session, err := cli.NewSession()
	if err != nil {
		w.Log("New session error: %v", err)
		return err
	}
	defer session.Close()
	go func() {
		<-ctx.Done()
		session.Close()
		cli.Close()
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

func (w *wsHandler) remoteInstallKubevpnIfCommandNotFound(ctx context.Context, sshConfig *util.SshConfig) error {
	cmd := `hash kubevpn || type kubevpn || which kubevpn || command -v kubevpn`
	_, _, err := util.RemoteRun(sshConfig, cmd, nil)
	if err == nil {
		w.Log("Remote kubevpn command found, not needs to install")
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
	fmt.Printf("The latest version is: %s, commit: %s\n", latestVersion, latestCommit)
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
	err = util.SCP(w.conn, w.conn, sshConfig, tempBin.Name(), "kubevpn", cmds...)
	if err != nil {
		return err
	}
	// try to startup daemon process
	go util.RemoteRun(sshConfig, "kubevpn get pods", nil)
	return nil
}

func (w *wsHandler) Log(format string, a ...any) {
	str := format
	if len(a) != 0 {
		str = fmt.Sprintf(format, a...)
	}
	w.conn.Write([]byte(str + "\r\n"))
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
		h.handle(conn.Request().Context())
	}))
	http.Handle("/resize", websocket.Handler(func(conn *websocket.Conn) {
		sessionID := conn.Request().Header.Get("session-id")
		log.Infof("resize: %s", sessionID)

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
