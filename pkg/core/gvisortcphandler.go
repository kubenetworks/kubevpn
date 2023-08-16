package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/pkg/config"
)

type gvisorTCPTunnelConnector struct {
}

func GvisorTCPTunnelConnector() Connector {
	return &gvisorTCPTunnelConnector{}
}

func (c *gvisorTCPTunnelConnector) ConnectContext(ctx context.Context, conn net.Conn) (net.Conn, error) {
	switch con := conn.(type) {
	case *net.TCPConn:
		err := con.SetNoDelay(true)
		if err != nil {
			return nil, err
		}
		err = con.SetKeepAlive(true)
		if err != nil {
			return nil, err
		}
		err = con.SetKeepAlivePeriod(15 * time.Second)
		if err != nil {
			return nil, err
		}
	}
	return conn, nil
}

type gvisorTCPHandler struct{}

func GvisorTCPHandler() Handler {
	return &gvisorTCPHandler{}
}

func (h *gvisorTCPHandler) Handle(ctx context.Context, tcpConn net.Conn) {
	defer tcpConn.Close()
	log.Debugf("[TUN-TCP] %s -> %s", tcpConn.RemoteAddr(), tcpConn.LocalAddr())
	// 1, get proxy info
	endpointID, err := ParseProxyInfo(tcpConn)
	if err != nil {
		log.Debugf("[TUN-TCP] Error: failed to parse proxy info: %v", err)
		return
	}
	log.Debugf("[TUN-TCP] Debug: LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress %s",
		endpointID.LocalPort, endpointID.LocalAddress.String(), endpointID.RemotePort, endpointID.RemoteAddress.String(),
	)
	// 2, dial proxy
	host := endpointID.LocalAddress.String()
	port := fmt.Sprintf("%d", endpointID.LocalPort)
	var remote net.Conn
	remote, err = net.DialTimeout("tcp", net.JoinHostPort(host, port), time.Second*5)
	if err != nil {
		log.Debugf("[TUN-TCP] Error: failed to connect addr %s: %v", net.JoinHostPort(host, port), err)
		return
	}

	errChan := make(chan error, 2)
	go func() {
		i := config.LPool.Get().([]byte)[:]
		defer config.LPool.Put(i[:])
		written, err2 := io.CopyBuffer(remote, tcpConn, i)
		log.Debugf("[TUN-TCP] Debug: write length %d data to remote", written)
		errChan <- err2
	}()
	go func() {
		i := config.LPool.Get().([]byte)[:]
		defer config.LPool.Put(i[:])
		written, err2 := io.CopyBuffer(tcpConn, remote, i)
		log.Debugf("[TUN-TCP] Debug: read length %d data from remote", written)
		errChan <- err2
	}()
	err = <-errChan
	if err != nil && !errors.Is(err, io.EOF) {
		log.Debugf("[TUN-TCP] Error: dsiconnect: %s >-<: %s: %v", tcpConn.LocalAddr(), remote.RemoteAddr(), err)
	}
}

func GvisorTCPListener(addr string) (net.Listener, error) {
	log.Debug("gvisor tcp listen addr", addr)
	laddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}
	ln, err := net.ListenTCP("tcp", laddr)
	if err != nil {
		return nil, err
	}
	return &tcpKeepAliveListener{TCPListener: ln}, nil
}
