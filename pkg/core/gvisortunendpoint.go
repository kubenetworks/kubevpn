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
	tcpConn, _ := newGvisorFakeUDPTunnelConnOverTCP(ctx, conn)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		pktBuffer := endpoint.ReadContext(ctx)
		if pktBuffer != nil {
			sniffer.LogPacket("[gVISOR] ", sniffer.DirectionSend, pktBuffer.NetworkProtocolNumber, pktBuffer)
			buf := pktBuffer.ToView().AsSlice()
			_, err := tcpConn.Write(buf)
			if err != nil {
				plog.G(ctx).Errorf("[TUN] Failed to write data to tun device: %v", err)
			}
		}
	}
}

// tun --> dispatcher
func (h *gvisorTCPHandler) readFromTCPConnWriteToEndpoint(ctx context.Context, conn net.Conn, endpoint *channel.Endpoint) {
	tcpConn, _ := newGvisorFakeUDPTunnelConnOverTCP(ctx, conn)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		buf := config.LPool.Get().([]byte)[:]
		read, err := tcpConn.Read(buf[:])
		if err != nil {
			plog.G(ctx).Errorf("[TUN] Failed to read from tcp conn: %v", err)
			config.LPool.Put(buf[:])
			return
		}
		if read == 0 {
			plog.G(ctx).Warnf("[TUN] Read from tcp conn length is %d", read)
			config.LPool.Put(buf[:])
			continue
		}
		// Try to determine network protocol number, default zero.
		var protocol tcpip.NetworkProtocolNumber
		var ipProtocol int
		var src, dst net.IP
		// TUN interface with IFF_NO_PI enabled, thus
		// we need to determine protocol from version field
		if util.IsIPv4(buf) {
			protocol = header.IPv4ProtocolNumber
			ipHeader, err := ipv4.ParseHeader(buf[:read])
			if err != nil {
				plog.G(ctx).Errorf("Failed to parse IPv4 header: %v", err)
				config.LPool.Put(buf[:])
				continue
			}
			ipProtocol = ipHeader.Protocol
			src = ipHeader.Src
			dst = ipHeader.Dst
		} else if util.IsIPv6(buf) {
			protocol = header.IPv6ProtocolNumber
			ipHeader, err := ipv6.ParseHeader(buf[:read])
			if err != nil {
				plog.G(ctx).Errorf("Failed to parse IPv6 header: %s", err.Error())
				config.LPool.Put(buf[:])
				continue
			}
			ipProtocol = ipHeader.NextHeader
			src = ipHeader.Src
			dst = ipHeader.Dst
		} else {
			plog.G(ctx).Debugf("[TUN-GVISOR] Unknown packet")
			config.LPool.Put(buf[:])
			continue
		}

		h.addRoute(ctx, src, conn)
		// inner ip like 198.19.0.100/102/103 connect each other
		if config.CIDR.Contains(dst) || config.CIDR6.Contains(dst) {
			plog.G(ctx).Debugf("[TUN-RAW] Forward to TUN device, SRC: %s, DST: %s, Length: %d", src.String(), dst.String(), read)
			util.SafeWrite(h.packetChan, &DatagramPacket{
				DataLength: uint16(read),
				Data:       buf[:],
			})
			continue
		}

		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			ReserveHeaderBytes: 0,
			Payload:            buffer.MakeWithData(buf[:read]),
		})
		config.LPool.Put(buf[:])
		sniffer.LogPacket("[gVISOR] ", sniffer.DirectionRecv, protocol, pkt)
		endpoint.InjectInbound(protocol, pkt)
		pkt.DecRef()
		plog.G(ctx).Debugf("[TUN-%s] Write to Gvisor IP-Protocol: %s, SRC: %s, DST: %s, Length: %d", layers.IPProtocol(ipProtocol).String(), layers.IPProtocol(ipProtocol).String(), src.String(), dst, read)
	}
}

func (h *gvisorTCPHandler) addRoute(ctx context.Context, src net.IP, tcpConn net.Conn) {
	value, loaded := h.routeMapTCP.LoadOrStore(src.String(), tcpConn)
	if loaded {
		if tcpConn != value.(net.Conn) {
			h.routeMapTCP.Store(src.String(), tcpConn)
			plog.G(ctx).Debugf("[TCP] Replace route map TCP: %s -> %s-%s", src, tcpConn.LocalAddr(), tcpConn.RemoteAddr())
		}
	} else {
		plog.G(ctx).Debugf("[TCP] Add new route map TCP: %s -> %s-%s", src, tcpConn.LocalAddr(), tcpConn.RemoteAddr())
	}
}
