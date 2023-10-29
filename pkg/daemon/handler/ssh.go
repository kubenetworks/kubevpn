package handler

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/terminal"
	"golang.org/x/net/websocket"
	"golang.org/x/oauth2"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/core"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

// Ws
// 0) remote server install kubevpn if not found
// 1) start remote kubevpn server
// 2) start local tunnel
// 3) ssh terminal
func Ws(conn *websocket.Conn, sshConfig *util.SshConfig) {
	var ctx = context.Background()
	cancel, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()

	err := remoteInstallKubevpnIfCommandNotFound(ctx, sshConfig)
	if err != nil {
		return
	}

	clientIP, err := util.GetIPBaseNic()
	if err != nil {
		return
	}

	local, err := portMap(cancel, sshConfig)
	if err != nil {
		return
	}
	cmd := fmt.Sprintf(`export %s=%s && kubevpn ssh-daemon --client-ip %s`, config.EnvStartSudoKubeVPNByKubeVPN, "true", clientIP.String())
	serverIP, stderr, err := util.RemoteRun(sshConfig, cmd, nil)
	if err != nil {
		log.Errorf("run error: %v", err)
		log.Errorf("run stdout: %v", string(serverIP))
		log.Errorf("run stderr: %v", string(stderr))
		return
	}
	ip, _, err := net.ParseCIDR(string(serverIP))
	if err != nil {
		return
	}
	r := core.Route{
		ServeNodes: []string{
			fmt.Sprintf("tun:/127.0.0.1:8422?net=%s&route=%s", clientIP, string(serverIP)),
		},
		ChainNode: fmt.Sprintf("tcp://127.0.0.1:%d", local),
		Retries:   5,
	}
	servers, err := handler.Parse(r)
	if err != nil {
		log.Errorf("parse route error: %v", err)
		return
	}
	go func() {
		log.Error(handler.Run(cancel, servers))
	}()
	tun, err := util.GetTunDevice(clientIP.IP)
	if err != nil {
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
	err = enterTerminal(sshConfig, conn)
	return
}

func portMap(ctx context.Context, conf *util.SshConfig) (localPort int, err error) {
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
		log.Errorf("ssh proxy err: %v", err)
		return
	}
}

func enterTerminal(conf *util.SshConfig, conn *websocket.Conn) error {
	cli, err := util.DialSshRemote(conf)
	if err != nil {
		return err
	}
	session, err := cli.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	session.Stdout = conn
	session.Stderr = conn
	session.Stdin = conn

	fd := int(os.Stdin.Fd())
	state, err := terminal.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("terminal make raw: %s", err)
	}
	defer terminal.Restore(fd, state)

	w, h, err := terminal.GetSize(fd)
	if err != nil {
		return fmt.Errorf("terminal get size: %s", err)
	}
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.ECHOCTL:       0,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm", h, w, modes); err != nil {
		return err
	}
	if err = session.Shell(); err != nil {
		return err
	}
	return session.Wait()
}

func remoteInstallKubevpnIfCommandNotFound(ctx context.Context, sshConfig *util.SshConfig) error {
	cmd := `hash kubevpn || type kubevpn || which kubevpn || command -v kubevpn`
	_, _, err := util.RemoteRun(sshConfig, cmd, nil)
	if err == nil {
		return nil
	}
	log.Infof("remote kubevpn command not found, try to install it...")
	var client = http.DefaultClient
	if config.GitHubOAuthToken != "" {
		client = oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: config.GitHubOAuthToken, TokenType: "Bearer"}))
	}
	latestVersion, latestCommit, url, err := util.GetManifest(client, "linux", "amd64")
	if err != nil {
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
	err = util.Download(client, url, temp.Name())
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
	err = util.SCP(sshConfig, tempBin.Name(), "/usr/local/bin/kubevpn")
	if err != nil {
		return err
	}
	// try to startup daemon process
	go util.RemoteRun(sshConfig, "kubevpn get pods", nil)
	return nil
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
		Ws(conn, &sshConfig)
	}))
}
