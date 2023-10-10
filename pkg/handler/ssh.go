package handler

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/core"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

// SSH
// 0) remote server install kubevpn if not found
// 1) start remote kubevpn server
// 2) start local tunnel
// 3) ssh terminal
func SSH(ctx context.Context, sshConfig *util.SshConfig) error {
	var clientIP = "223.254.0.124/32"

	cancel, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()
	local, err := portMap(ctx, sshConfig)
	if err != nil {
		return err
	}
	cmd := fmt.Sprintf(`export %s=%s && kubevpn ssh-daemon --client-ip %s`, config.EnvStartSudoKubeVPNByKubeVPN, "true", clientIP)
	env := map[string]string{config.EnvStartSudoKubeVPNByKubeVPN: "true"}
	serverIP, stderr, err := util.RemoteRun(sshConfig, cmd, env)
	if err != nil {
		log.Errorf("run error: %v", err)
		log.Errorf("run stdout: %v", string(serverIP))
		log.Errorf("run stderr: %v", string(stderr))
		return err
	}
	r := core.Route{
		ServeNodes: []string{
			fmt.Sprintf("tun:/127.0.0.1:8422?net=%s&route=%s", clientIP, string(serverIP)),
		},
		ChainNode: fmt.Sprintf("tcp://127.0.0.1:%d", local),
		Retries:   5,
	}
	servers, err := Parse(r)
	if err != nil {
		log.Errorf("parse route error: %v", err)
		return err
	}
	go func() {
		log.Error(Run(cancel, servers))
	}()
	log.Info("tunnel connected")
	go func() {
		ticker := time.NewTicker(time.Second * 2)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = exec.CommandContext(ctx, "ping", "-c", "4", "223.254.0.124").Run()
		}
	}()
	<-cancel.Done()
	return err
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
