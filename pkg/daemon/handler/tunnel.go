package handler

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	pkgssh "github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

func (w *wsHandler) createTunnel(ctx context.Context, cli *ssh.Client) error {
	if err := w.installKubevpnOnRemote(ctx, cli); err != nil {
		return err
	}

	clientIP, err := netutil.GetLocalIPNet()
	if err != nil {
		w.log("Get client IP error: %v", err)
		return err
	}

	localPort, err := util.GetAvailableTCPPort()
	if err != nil {
		return err
	}
	remote, _ := netip.ParseAddrPort(net.JoinHostPort("127.0.0.1", strconv.Itoa(config.PortTCP)))
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
	w.log("%s", util.FormatBanner(fmt.Sprintf("You can use client: %s to communicate with server: %s", clientIP.IP.String(), ip.String())))
	w.cidr = append(w.cidr, string(serverIP))

	nodes := []*core.Node{
		core.NewNode("tun", "").
			WithForward(fmt.Sprintf("tcp://127.0.0.1:%d", localPort)).
			WithParam("net", clientIP.String()).
			WithParam("route", strings.Join(w.cidr, ",")),
	}
	servers, err := core.GenerateServersFromNodes(nodes, core.NewRouteHub())
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

	// Align with the core data-plane heartbeat cadence (config.KeepAliveTime) instead of
	// running a separate, more aggressive keepalive loop.
	go func() {
		for ctx.Err() == nil {
			_, _ = netutil.Ping(ctx, clientIP.IP.String(), ip.String())
			time.Sleep(config.KeepAliveTime)
		}
	}()
	return nil
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
