package handler

import (
	"context"
	"fmt"
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
	"golang.org/x/crypto/ssh/terminal"
	"golang.org/x/net/websocket"
	"golang.org/x/oauth2"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/core"
	"github.com/wencaiwulue/kubevpn/pkg/errors"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

type wsHandler struct {
	conn      *websocket.Conn
	sshConfig *util.SshConfig
	cidr      []string
}

// handle
// 0) remote server install kubevpn if not found
// 1) start remote kubevpn server
// 2) start local tunnel
// 3) ssh terminal
func (w *wsHandler) handle(ctx context.Context) {
	conn := w.conn
	sshConfig := w.sshConfig
	cidr := w.cidr
	cancel, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()

	err := w.remoteInstallKubevpnIfCommandNotFound(ctx, sshConfig)
	if err != nil {
		err = errors.Wrap(err, "Failed to install Kubevpn remotely if command not found ")
		w.Log("Install kubevpn error: %v", err)
		return
	}

	clientIP, err := util.GetIPBaseNic()
	if err != nil {
		err = errors.Wrap(err, "Failed to get IP base NIC ")
		w.Log("Get client ip error: %v", err)
		return
	}

	local, err := w.portMap(cancel, sshConfig)
	if err != nil {
		err = errors.Wrap(err, "Failed to map the port with SSH configuration.")
		w.Log("Port map error: %v", err)
		return
	}
	cmd := fmt.Sprintf(`export %s=%s && kubevpn ssh-daemon --client-ip %s`, config.EnvStartSudoKubeVPNByKubeVPN, "true", clientIP.String())
	serverIP, stderr, err := util.RemoteRun(sshConfig, cmd, nil)
	if err != nil {
		errors.LogErrorf("run error: %v", err)
		errors.LogErrorf("run stdout: %v", string(serverIP))
		errors.LogErrorf("run stderr: %v", string(stderr))
		w.Log("Start kubevpn server error: %v", err)
		return
	}
	ip, _, err := net.ParseCIDR(string(serverIP))
	if err != nil {
		err = errors.Wrap(err, "Failed to parse the server IP.")
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
		errors.LogErrorf("parse route error: %v", err)
		w.Log("Parse route error: %v", err)
		return
	}
	go func() {
		log.Error(handler.Run(cancel, servers))
	}()
	tun, err := util.GetTunDevice(clientIP.IP)
	if err != nil {
		err = errors.Wrap(err, "Failed to get the TUN device.")
		w.Log("Get tun device error: %v", err)
		return
	}
	log.Info("tunnel connected")
	go func() {
		ticker := time.NewTicker(time.Second * 2)
		for {
			select {
			case <-cancel.Done():
				return
			case <-ticker.C:
				_, _ = util.Ping(clientIP.IP.String())
				_, _ = util.Ping(ip.String())
				_ = exec.CommandContext(cancel, "ping", "-c", "4", "-b", tun.Name, ip.String()).Run()
			}
		}
	}()
	err = w.enterTerminal(sshConfig, conn)
	return
}

func (w *wsHandler) portMap(ctx context.Context, conf *util.SshConfig) (localPort int, err error) {
	removePort := 10800
	localPort, err = util.GetAvailableTCPPortOrDie()
	if err != nil {
		err = errors.Wrap(err, "Failed to get an available TCP port.")
		return
	}
	var remote netip.AddrPort
	remote, err = netip.ParseAddrPort(net.JoinHostPort("127.0.0.1", strconv.Itoa(removePort)))
	if err != nil {
		err = errors.Wrap(err, "Failed to parse the address and port.")
		return
	}
	var local netip.AddrPort
	local, err = netip.ParseAddrPort(net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort)))
	if err != nil {
		err = errors.Wrap(err, "netip.ParseAddrPort(net.JoinHostPort(\"127.0.0.1\", strconv.Itoa(localPort))): ")
		return
	}

	// pre-check network ip connect
	var cli *ssh.Client
	cli, err = util.DialSshRemote(conf)
	if err != nil {
		err = errors.Wrap(err, "Failed to dial SSH remote.")
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
					errors.LogErrorf("ssh forward failed err: %v", err)
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
		errors.LogErrorf("ssh proxy err: %v", err)
		w.Log("Ssh forward failed err: %v", err)
		return
	}
}

func (w *wsHandler) enterTerminal(conf *util.SshConfig, conn *websocket.Conn) error {
	cli, err := util.DialSshRemote(conf)
	if err != nil {
		err = errors.Wrap(err, "Failed to dial SSH remote.")
		w.Log("Dial remote error: %v", err)
		return err
	}
	session, err := cli.NewSession()
	if err != nil {
		err = errors.Wrap(err, "Failed to create a new session.")
		return err
	}
	defer session.Close()
	session.Stdout = conn
	session.Stderr = conn
	session.Stdin = conn

	fd := int(os.Stdin.Fd())
	state, err := terminal.MakeRaw(fd)
	if err != nil {
		return errors.Errorf("terminal make raw: %s", err)
	}
	defer terminal.Restore(fd, state)

	width, height, err := terminal.GetSize(fd)
	if err != nil {
		w.Log("Terminal get size: %s", err)
		return errors.Errorf("terminal get size: %s", err)
	}
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.ECHOCTL:       0,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm", height, width, modes); err != nil {
		return err
	}
	if err = session.Shell(); err != nil {
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
		err = errors.Wrap(err, "Failed to get the manifest.")
		w.Log("Get latest kubevpn version failed: %v", err)
		return err
	}
	fmt.Printf("The latest version is: %s, commit: %s\n", latestVersion, latestCommit)
	var temp *os.File
	temp, err = os.CreateTemp("", "")
	if err != nil {
		err = errors.Wrap(err, "Failed to create a temporary file.")
		return err
	}
	err = temp.Close()
	if err != nil {
		err = errors.Wrap(err, "Failed to close the temporary file.")
		return err
	}
	w.Log("Downloading kubevpn...")
	err = util.Download(client, url, temp.Name(), w.conn, w.conn)
	if err != nil {
		err = errors.Wrap(err, "Failed to download the file.")
		return err
	}
	var tempBin *os.File
	tempBin, err = os.CreateTemp("", "kubevpn")
	if err != nil {
		err = errors.Wrap(err, "Failed to create a temporary file for KubeVPN.")
		return err
	}
	err = tempBin.Close()
	if err != nil {
		err = errors.Wrap(err, "Failed to close the temporary binary file.")
		return err
	}
	err = util.UnzipKubeVPNIntoFile(temp.Name(), tempBin.Name())
	if err != nil {
		err = errors.Wrap(err, "Failed to unzip KubeVPN into the file.")
		return err
	}
	// scp kubevpn to remote ssh server and run daemon
	err = os.Chmod(tempBin.Name(), 0755)
	if err != nil {
		err = errors.Wrap(err, "Failed to change the mode of the file.")
		return err
	}
	err = os.Remove(temp.Name())
	if err != nil {
		err = errors.Wrap(err, "Failed to remove the temporary file.")
		return err
	}
	log.Infof("Upgrade daemon...")
	w.Log("Scp kubevpn to remote server ~/.kubevpn/kubevpn")
	cmds := []string{
		"chmod +x ~/.kubevpn/kubevpn",
		"mv ~/.kubevpn/kubevpn /usr/local/bin/kubevpn",
	}
	err = util.SCP(w.conn, w.conn, sshConfig, tempBin.Name(), "kubevpn", cmds...)
	if err != nil {
		err = errors.Wrap(err, "Failed to copy the file using SCP.")
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
	w.conn.Write([]byte(str + "\n"))
}

func (w *wsHandler) PrintLine(msg string) {
	line := "+" + strings.Repeat("-", len(msg)-2) + "+"
	w.Log(line)
	w.Log(msg)
	w.Log(line)
}

func init() {
	http.Handle("/ws", websocket.Handler(func(conn *websocket.Conn) {
		sshConfig := util.SshConfig{
			Addr:        conn.Request().Header.Get("ssh-addr"),
			User:        conn.Request().Header.Get("ssh-username"),
			Password:    conn.Request().Header.Get("ssh-password"),
			Keyfile:     conn.Request().Header.Get("ssh-keyfile"),
			ConfigAlias: conn.Request().Header.Get("ssh-alias"),
		}
		var extraCIDR []string
		if v := conn.Request().Header.Get("extra-cidr"); v != "" {
			extraCIDR = strings.Split(v, ",")
		}
		h := &wsHandler{sshConfig: &sshConfig, conn: conn, cidr: extraCIDR}
		h.handle(context.Background())
	}))
}
