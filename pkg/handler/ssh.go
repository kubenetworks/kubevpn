package handler

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/crypto/ssh"
	"net"
	"net/netip"
	"strconv"

	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/pkg/core"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

// SSH
// 0) remote server install kubevpn if not found
// 1) start remote kubevpn server
// 2) start local tunnel
// 3) ssh terminal
func SSH(ctx context.Context, config *util.SshConfig) error {
	cancel, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()
	err := portMap(ctx, config)
	if err != nil {
		return err
	}
	go func() {
		stdout, stderr, err := util.RemoteRun(config, fmt.Sprintf(`kubevpn serve -L "tcp://:10800" -L "tun://127.0.0.1:8422?net=223.254.0.123/32"`), nil)
		if err != nil {
			log.Errorf("run error: %v", err)
			log.Errorf("run stdout: %v", string(stdout))
			log.Errorf("run stderr: %v", string(stderr))
			cancelFunc()
		}
	}()

	r := core.Route{
		ServeNodes: []string{
			fmt.Sprintf("tun:/127.0.0.1:8422?net=%s&route=%s", "223.254.0.124/32", "223.254.0.124/32"),
		},
		ChainNode: "tcp://172.17.64.35:10800",
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
	<-cancel.Done()
	return err
}

func portMap(ctx context.Context, conf *util.SshConfig) (err error) {
	port := 10800
	var remote netip.AddrPort
	remote, err = netip.ParseAddrPort(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return
	}
	var local netip.AddrPort
	local, err = netip.ParseAddrPort(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
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
