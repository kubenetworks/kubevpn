package core

import (
	"context"
	"net"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type fakeUDPTunnelConnector struct {
}

func UDPOverTCPTunnelConnector() Connector {
	return &fakeUDPTunnelConnector{}
}

func (c *fakeUDPTunnelConnector) ConnectContext(ctx context.Context, conn net.Conn) (net.Conn, error) {
	//defer conn.SetDeadline(time.Time{})
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
	return newFakeUDPTunnelConnOverTCP(ctx, conn)
}

type fakeUdpHandler struct {
	// map[srcIP]net.Conn
	connNAT *sync.Map
	ch      chan *datagramPacket
}

func TCPHandler() Handler {
	return &fakeUdpHandler{
		connNAT: RouteConnNAT,
		ch:      Chan,
	}
}

func (h *fakeUdpHandler) Handle(ctx context.Context, tcpConn net.Conn) {
	defer tcpConn.Close()
	log.Debugf("[tcpserver] %s -> %s\n", tcpConn.RemoteAddr(), tcpConn.LocalAddr())

	defer func(addr net.Addr) {
		var keys []string
		h.connNAT.Range(func(key, value any) bool {
			if value.(net.Conn) == tcpConn {
				keys = append(keys, key.(string))
			}
			return true
		})
		for _, key := range keys {
			h.connNAT.Delete(key)
		}
		log.Debugf("[tcpserver] delete conn %s from globle routeConnNAT, deleted count %d", addr, len(keys))
	}(tcpConn.LocalAddr())

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		b := config.LPool.Get().([]byte)[:]
		dgram, err := readDatagramPacketServer(tcpConn, b[:])
		if err != nil {
			log.Debugf("[tcpserver] %s -> 0 : %v", tcpConn.RemoteAddr(), err)
			return
		}

		var src net.IP
		bb := dgram.Data[:dgram.DataLength]
		if util.IsIPv4(bb) {
			src = net.IPv4(bb[12], bb[13], bb[14], bb[15])
		} else if util.IsIPv6(bb) {
			src = bb[8:24]
		} else {
			log.Errorf("[tcpserver] unknown packet")
			continue
		}
		value, loaded := h.connNAT.LoadOrStore(src.String(), tcpConn)
		if loaded {
			if tcpConn != value.(net.Conn) {
				h.connNAT.Store(src.String(), tcpConn)
				log.Debugf("[tcpserver] replace routeConnNAT: %s -> %s-%s", src, tcpConn.LocalAddr(), tcpConn.RemoteAddr())
			}
			log.Debugf("[tcpserver] find routeConnNAT: %s -> %s-%s", src, tcpConn.LocalAddr(), tcpConn.RemoteAddr())
		} else {
			log.Debugf("[tcpserver] new routeConnNAT: %s -> %s-%s", src, tcpConn.LocalAddr(), tcpConn.RemoteAddr())
		}
		util.SafeWrite(h.ch, dgram)
	}
}

// fake udp connect over tcp
type fakeUDPTunnelConn struct {
	// tcp connection
	net.Conn
	ctx context.Context
}

func newFakeUDPTunnelConnOverTCP(ctx context.Context, conn net.Conn) (net.Conn, error) {
	return &fakeUDPTunnelConn{ctx: ctx, Conn: conn}, nil
}

func (c *fakeUDPTunnelConn) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case <-c.ctx.Done():
		return 0, nil, c.ctx.Err()
	default:
		dgram, err := readDatagramPacket(c.Conn, b)
		if err != nil {
			return 0, nil, err
		}
		return int(dgram.DataLength), dgram.Addr(), nil
	}
}

func (c *fakeUDPTunnelConn) WriteTo(b []byte, _ net.Addr) (int, error) {
	dgram := newDatagramPacket(b)
	if err := dgram.Write(c.Conn); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *fakeUDPTunnelConn) Close() error {
	if cc, ok := c.Conn.(interface{ CloseRead() error }); ok {
		_ = cc.CloseRead()
	}
	if cc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		_ = cc.CloseWrite()
	}
	return c.Conn.Close()
}
