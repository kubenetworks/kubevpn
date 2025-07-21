package core

import (
	"context"
	"net"

	"github.com/google/gopacket/layers"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/sniffer"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (h *gvisorTCPHandler) readFromEndpointWriteToTCPConn(ctx context.Context, conn net.Conn, endpoint *channel.Endpoint) {
	tcpConn, _ := newGvisorUDPConnOverTCP(ctx, conn)
	for ctx.Err() == nil {
		pktBuffer := endpoint.ReadContext(ctx)
		if pktBuffer != nil {
			sniffer.LogPacket("[gVISOR] ", sniffer.DirectionSend, pktBuffer.NetworkProtocolNumber, pktBuffer)
			data := pktBuffer.ToView().AsSlice()
			buf := config.LPool.Get().([]byte)[:]
			n := copy(buf[1:], data)
			buf[0] = 0
			_, err := tcpConn.Write(buf[:n+1])
			config.LPool.Put(buf[:])
			if err != nil {
				plog.G(ctx).Errorf("[TUN-GVISOR] Failed to write data to tun device: %v", err)
			}
		}
	}
}

// tun --> dispatcher
func (h *gvisorTCPHandler) readFromTCPConnWriteToEndpoint(ctx context.Context, conn net.Conn, endpoint *channel.Endpoint) {
	tcpConn, _ := newGvisorUDPConnOverTCP(ctx, conn)
	defer h.removeFromRouteMapTCP(ctx, conn)

	for ctx.Err() == nil {
		buf := config.LPool.Get().([]byte)[:]
		read, err := tcpConn.Read(buf[:])
		if err != nil {
			plog.G(ctx).Errorf("[TCP-GVISOR] Failed to read from tcp conn: %v", err)
			config.LPool.Put(buf[:])
			return
		}
		if read == 0 {
			plog.G(ctx).Warnf("[TCP-GVISOR] Read from tcp conn length is %d", read)
			config.LPool.Put(buf[:])
			continue
		}
		// Try to determine network protocol number, default zero.
		var protocol tcpip.NetworkProtocolNumber
		var ipProtocol int
		var src, dst net.IP
		// TUN interface with IFF_NO_PI enabled, thus
		// we need to determine protocol from version field
		if util.IsIPv4(buf[1:read]) {
			protocol = header.IPv4ProtocolNumber
			ipHeader, err := ipv4.ParseHeader(buf[1:read])
			if err != nil {
				plog.G(ctx).Errorf("Failed to parse IPv4 header: %v", err)
				config.LPool.Put(buf[:])
				continue
			}
			ipProtocol = ipHeader.Protocol
			src = ipHeader.Src
			dst = ipHeader.Dst
		} else if util.IsIPv6(buf[1:read]) {
			protocol = header.IPv6ProtocolNumber
			ipHeader, err := ipv6.ParseHeader(buf[1:read])
			if err != nil {
				plog.G(ctx).Errorf("[TCP-GVISOR] Failed to parse IPv6 header: %s", err.Error())
				config.LPool.Put(buf[:])
				continue
			}
			ipProtocol = ipHeader.NextHeader
			src = ipHeader.Src
			dst = ipHeader.Dst
		} else {
			plog.G(ctx).Errorf("[TCP-GVISOR] Unknown packet")
			config.LPool.Put(buf[:])
			continue
		}

		h.addToRouteMapTCP(ctx, src, conn)
		// inner ip like 198.19.0.100/102/103 connect each other
		// for issue 594, sometimes k8s service network CIDR also use CIDR 198.19.151.170
		// if we can find dst in route map, just trade packet as inner communicate
		// if not find dst in route map, just trade packet as k8s service/pod ip
		if c, found := h.routeMapTCP.Load(dst.String()); found {
			plog.G(ctx).Debugf("[TCP-GVISOR] Find TCP route SRC: %s to DST: %s -> %s", src, dst, c.(net.Conn).RemoteAddr())
			dgram := newDatagramPacket(buf, read)
			err = dgram.Write(c.(net.Conn))
			config.LPool.Put(buf[:])
			if err != nil {
				plog.G(ctx).Errorf("[TCP-GVISOR] Failed to write to %s <- %s : %s", c.(net.Conn).RemoteAddr(), c.(net.Conn).LocalAddr(), err)
			}
		} else if buf[0] == 1 {
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
				ReserveHeaderBytes: 0,
				Payload:            buffer.MakeWithData(buf[1:read]),
			})
			config.LPool.Put(buf[:])
			sniffer.LogPacket("[gVISOR] ", sniffer.DirectionRecv, protocol, pkt)
			endpoint.InjectInbound(protocol, pkt)
			pkt.DecRef()
			plog.G(ctx).Debugf("[TCP-GVISOR] Write to Gvisor. SRC: %s, DST: %s, Protocol: %s, Length: %d", src, dst, layers.IPProtocol(ipProtocol).String(), read)
		} else {
			util.SafeWrite(TCPPacketChan, &Packet{
				data:   buf[:],
				length: read,
				src:    src,
				dst:    dst,
			}, func(v *Packet) {
				config.LPool.Put(buf[:])
				plog.G(ctx).Debugf("[TCP-TUN] Drop packet. SRC: %s, DST: %s, Protocol: %s, Length: %d", src, dst, layers.IPProtocol(ipProtocol).String(), read)
			})
		}
	}
}

func (h *gvisorTCPHandler) addToRouteMapTCP(ctx context.Context, src net.IP, tcpConn net.Conn) {
	value, loaded := h.routeMapTCP.LoadOrStore(src.String(), tcpConn)
	if loaded {
		if value.(net.Conn) != tcpConn {
			h.routeMapTCP.Store(src.String(), tcpConn)
			plog.G(ctx).Infof("[TUN-GVISOR] Replace route map TCP: %s -> %s-%s", src, tcpConn.LocalAddr(), tcpConn.RemoteAddr())
		}
	} else {
		plog.G(ctx).Infof("[TUN-GVISOR] Add new route map TCP: %s -> %s-%s", src, tcpConn.LocalAddr(), tcpConn.RemoteAddr())
	}
}

func (h *gvisorTCPHandler) removeFromRouteMapTCP(ctx context.Context, tcpConn net.Conn) {
	h.routeMapTCP.Range(func(key, value any) bool {
		if value.(net.Conn) == tcpConn {
			h.routeMapTCP.Delete(key)
			plog.G(ctx).Infof("[TCP-GVISOR] Delete to DST %s by conn %s from globle route map TCP", key, tcpConn.LocalAddr())
		}
		return true
	})
}
