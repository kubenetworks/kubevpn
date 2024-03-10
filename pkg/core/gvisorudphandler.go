package core

import (
	"context"
	"io"
	"net"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type gvisorUDPOverTCPTunnelConnector struct{}

func GvisorUDPOverTCPTunnelConnector() Connector {
	return &gvisorUDPOverTCPTunnelConnector{}
}

func (c *gvisorUDPOverTCPTunnelConnector) ConnectContext(ctx context.Context, conn net.Conn) (net.Conn, error) {
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

type gvisorUDPHandler struct{}

func GvisorUDPHandler() Handler {
	return &gvisorUDPHandler{}
}

func (h *gvisorUDPHandler) Handle(ctx context.Context, tcpConn net.Conn) {
	defer tcpConn.Close()
	log.Debugf("[TUN-UDP] %s -> %s", tcpConn.RemoteAddr(), tcpConn.LocalAddr())
	// 1, get proxy info
	endpointID, err := ParseProxyInfo(tcpConn)
	if err != nil {
		log.Warningf("[TUN-UDP] Error: Failed to parse proxy info: %v", err)
		return
	}
	log.Debugf("[TUN-UDP] Debug: LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress %s",
		endpointID.LocalPort, endpointID.LocalAddress.String(), endpointID.RemotePort, endpointID.RemoteAddress.String(),
	)
	// 2, dial proxy
	addr := &net.UDPAddr{
		IP:   endpointID.LocalAddress.AsSlice(),
		Port: int(endpointID.LocalPort),
	}
	var remote *net.UDPConn
	remote, err = net.DialUDP("udp", nil, addr)
	if err != nil {
		log.Debugf("[TUN-UDP] Error: failed to connect addr %s: %v", addr.String(), err)
		return
	}
	handle(ctx, tcpConn, remote)
}

func GvisorUDPListener(addr string) (net.Listener, error) {
	log.Debugf("gvisor UDP over TCP listen addr: %s", addr)
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

func handle(ctx context.Context, tcpConn net.Conn, udpConn *net.UDPConn) {
	defer udpConn.Close()
	log.Debugf("[TUN-UDP] Debug: %s <-> %s", tcpConn.RemoteAddr(), udpConn.RemoteAddr())
	errChan := make(chan error, 2)
	tcpC := util.NewReadWriter(tcpConn, time.Second*30)
	udpC := util.NewReadWriter(udpConn, time.Second*30)
	go func() {
		buf := config.LPool.Get().([]byte)[:]
		defer config.LPool.Put(buf[:])
		written, err := io.CopyBuffer(udpC, tcpC, buf[:])
		errChan <- err
		log.Debugf("[TUN-UDP] Debug: %s >>> %s length: %d", tcpConn.RemoteAddr(), udpConn.RemoteAddr(), written)
	}()

	go func() {
		buf := config.LPool.Get().([]byte)[:]
		defer config.LPool.Put(buf[:])
		written, err := io.CopyBuffer(tcpC, udpC, buf[:])
		errChan <- err
		log.Debugf("[TUN-UDP] Debug: %s <<< %s length: %d", tcpConn.RemoteAddr(), udpConn.RemoteAddr(), written)
	}()
	err := <-errChan
	if err != nil {
		log.Debugf("[TUN-UDP] Error: %v", err)
	}
	log.Debugf("[TUN-UDP] Debug: %s >-< %s", tcpConn.RemoteAddr(), udpConn.LocalAddr())
	return
}
