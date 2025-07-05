package core

import (
	"context"
	"net"
	"sync"

	"github.com/google/gopacket/layers"

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
	return NewUDPConnOverTCP(ctx, conn)
}

type UDPOverTCPHandler struct {
	// map[srcIP]net.Conn
	routeMapTCP *sync.Map
	packetChan  chan *Packet
}

func TCPHandler() Handler {
	return &UDPOverTCPHandler{
		routeMapTCP: RouteMapTCP,
		packetChan:  TCPPacketChan,
	}
}

func (h *UDPOverTCPHandler) Handle(ctx context.Context, tcpConn net.Conn) {
	tcpConn = NewBufferedTCP(tcpConn)
	defer tcpConn.Close()
	plog.G(ctx).Infof("[TCP] Handle connection %s -> %s", tcpConn.RemoteAddr(), tcpConn.LocalAddr())

	defer h.removeFromRouteMapTCP(ctx, tcpConn)

	for ctx.Err() == nil {
		buf := config.LPool.Get().([]byte)[:]
		datagram, err := readDatagramPacket(tcpConn, buf)
		if err != nil {
			plog.G(ctx).Errorf("[TCP] Failed to read from %s -> %s: %v", tcpConn.RemoteAddr(), tcpConn.LocalAddr(), err)
			config.LPool.Put(buf[:])
			return
		}

		err = h.handlePacket(ctx, tcpConn, datagram)
		if err != nil {
			return
		}
	}
}

func (h *UDPOverTCPHandler) handlePacket(ctx context.Context, tcpConn net.Conn, datagram *DatagramPacket) error {
	src, dst, protocol, err := util.ParseIP(datagram.Data[1:datagram.DataLength])
	if err != nil {
		plog.G(ctx).Errorf("[TCP] Unknown packet")
		config.LPool.Put(datagram.Data[:])
		return err
	}

	h.addToRouteMapTCP(ctx, src, tcpConn)

	if conn, ok := h.routeMapTCP.Load(dst.String()); ok {
		plog.G(ctx).Debugf("[TCP] Find TCP route SRC: %s to DST: %s -> %s", src, dst, conn.(net.Conn).RemoteAddr())
		// local client needs handle it with gVisor
		datagram.Data[0] = 1
		err = datagram.Write(conn.(net.Conn))
		config.LPool.Put(datagram.Data[:])
		if err != nil {
			plog.G(ctx).Errorf("[TCP] Failed to write to %s <- %s : %s", conn.(net.Conn).RemoteAddr(), conn.(net.Conn).LocalAddr(), err)
			return err
		}
	} else {
		plog.G(ctx).Debugf("[TCP] Forward to TUN device, SRC: %s, DST: %s, Protocol: %s, Length: %d", src, dst, layers.IPProtocol(protocol).String(), datagram.DataLength)
		util.SafeWrite(h.packetChan, NewPacket(datagram.Data, int(datagram.DataLength), src, dst), func(v *Packet) {
			plog.G(context.Background()).Errorf("Stuck packet, SRC: %s, DST: %s, Protocol: %s, Length: %d", src, dst, layers.IPProtocol(protocol).String(), v.length)
			h.packetChan <- v
		})
	}
	return nil
}

func (h *UDPOverTCPHandler) addToRouteMapTCP(ctx context.Context, src net.IP, tcpConn net.Conn) {
	value, loaded := h.routeMapTCP.LoadOrStore(src.String(), tcpConn)
	if loaded {
		if value.(net.Conn) != tcpConn {
			h.routeMapTCP.Store(src.String(), tcpConn)
			plog.G(ctx).Infof("[TCP] Replace route map TCP to DST %s by connation %s -> %s", src, tcpConn.RemoteAddr(), tcpConn.LocalAddr())
		}
	} else {
		plog.G(ctx).Infof("[TCP] Add new route map TCP to DST %s by connation %s -> %s", src, tcpConn.RemoteAddr(), tcpConn.LocalAddr())
	}
}

func (h *UDPOverTCPHandler) removeFromRouteMapTCP(ctx context.Context, tcpConn net.Conn) {
	h.routeMapTCP.Range(func(key, value any) bool {
		if value.(net.Conn) == tcpConn {
			h.routeMapTCP.Delete(key)
			plog.G(ctx).Infof("[TCP] Delete to DST: %s by conn %s -> %s from globle route map TCP", key, tcpConn.RemoteAddr(), tcpConn.LocalAddr())
		}
		return true
	})
}
