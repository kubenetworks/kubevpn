package core

import (
	"context"
	"net"
	"sync"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type UDPOverTCPConnector struct {
}

func NewUDPOverTCPConnector() Connector {
	return &UDPOverTCPConnector{}
}

func (c *UDPOverTCPConnector) ConnectContext(ctx context.Context, conn net.Conn) (net.Conn, error) {
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
		err = con.SetKeepAlivePeriod(config.KeepAliveTime)
		if err != nil {
			return nil, err
		}
	}
	return newUDPConnOverTCP(ctx, conn)
}

type UDPOverTCPHandler struct {
	// map[srcIP]net.Conn
	routeMapTCP *sync.Map
	packetChan  chan *DatagramPacket
}

func TCPHandler() Handler {
	return &UDPOverTCPHandler{
		routeMapTCP: RouteMapTCP,
		packetChan:  TCPPacketChan,
	}
}

func (h *UDPOverTCPHandler) Handle(ctx context.Context, tcpConn net.Conn) {
	defer tcpConn.Close()
	plog.G(ctx).Debugf("[TCP] %s -> %s", tcpConn.RemoteAddr(), tcpConn.LocalAddr())

	defer h.removeFromRouteMapTCP(ctx, tcpConn)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		buf := config.LPool.Get().([]byte)[:]
		packet, err := readDatagramPacketServer(tcpConn, buf[:])
		if err != nil {
			plog.G(ctx).Errorf("[TCP] %s -> %s : %v", tcpConn.RemoteAddr(), tcpConn.LocalAddr(), err)
			config.LPool.Put(buf[:])
			return
		}

		var src net.IP
		src, _, err = util.ParseIP(packet.Data[:packet.DataLength])
		if err != nil {
			plog.G(ctx).Errorf("[TCP] Unknown packet")
			config.LPool.Put(buf[:])
			continue
		}
		value, loaded := h.routeMapTCP.LoadOrStore(src.String(), tcpConn)
		if loaded {
			if tcpConn != value.(net.Conn) {
				h.routeMapTCP.Store(src.String(), tcpConn)
				plog.G(ctx).Debugf("[TCP] Replace route map TCP: %s -> %s-%s", src, tcpConn.LocalAddr(), tcpConn.RemoteAddr())
			}
		} else {
			plog.G(ctx).Debugf("[TCP] Add new route map TCP: %s -> %s-%s", src, tcpConn.LocalAddr(), tcpConn.RemoteAddr())
		}
		util.SafeWrite(h.packetChan, packet)
	}
}

func (h *UDPOverTCPHandler) removeFromRouteMapTCP(ctx context.Context, tcpConn net.Conn) {
	h.routeMapTCP.Range(func(key, value any) bool {
		if value.(net.Conn) == tcpConn {
			plog.G(ctx).Debugf("[TCP] Delete to DST: %s by conn %s from globle route map TCP", key, tcpConn.LocalAddr())
		}
		return true
	})
}

var _ net.PacketConn = (*UDPConnOverTCP)(nil)

// UDPConnOverTCP fake udp connection over tcp connection
type UDPConnOverTCP struct {
	// tcp connection
	net.Conn
	ctx context.Context
}

func newUDPConnOverTCP(ctx context.Context, conn net.Conn) (net.Conn, error) {
	return &UDPConnOverTCP{ctx: ctx, Conn: conn}, nil
}

func (c *UDPConnOverTCP) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case <-c.ctx.Done():
		return 0, nil, c.ctx.Err()
	default:
		packet, err := readDatagramPacket(c.Conn, b)
		if err != nil {
			return 0, nil, err
		}
		return int(packet.DataLength), nil, nil
	}
}

func (c *UDPConnOverTCP) WriteTo(b []byte, _ net.Addr) (int, error) {
	packet := newDatagramPacket(b)
	if err := packet.Write(c.Conn); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *UDPConnOverTCP) Close() error {
	if cc, ok := c.Conn.(interface{ CloseRead() error }); ok {
		_ = cc.CloseRead()
	}
	if cc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		_ = cc.CloseWrite()
	}
	return c.Conn.Close()
}
