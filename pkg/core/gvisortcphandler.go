package core

import (
	"context"
	"io"
	"net"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"
)

type gvisorTCPHandler struct{}

func GvisorTCPHandler() Handler {
	return &gvisorTCPHandler{}
}

func (h *gvisorTCPHandler) Handle(ctx context.Context, tcpConn net.Conn) {
	defer tcpConn.Close()
	log.Debugf("[GvisorTCPServer] %s -> %s\n", tcpConn.RemoteAddr(), tcpConn.LocalAddr())
	func(conn net.Conn) {
		defer conn.Close()
		// 1, get proxy info
		endpointID, err := ParseProxyInfo(conn)
		if err != nil {
			log.Warning("failed to parse proxy info", "err: ", err)
			return
		}
		log.Debugf("[TUN-TCP] Info: LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress %s",
			endpointID.LocalPort, endpointID.LocalAddress.String(), endpointID.RemotePort, endpointID.RemoteAddress.String(),
		)
		// 2, dial proxy
		s := net.ParseIP(endpointID.LocalAddress.String()).String()
		port := strconv.FormatUint(uint64(endpointID.LocalPort), 10)
		var dial net.Conn
		dial, err = net.DialTimeout("tcp", net.JoinHostPort(s, port), time.Second*5)
		if err != nil {
			log.Warningln(err)
			return
		}
		go io.Copy(conn, dial)
		io.Copy(dial, conn)
	}(tcpConn)
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
