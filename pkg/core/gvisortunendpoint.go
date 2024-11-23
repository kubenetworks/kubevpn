package core

import (
	"context"
	"net"

	"github.com/google/gopacket/layers"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/sniffer"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
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
				log.Errorf("[TUN] Failed to write data to tun device: %v", err)
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

		buf := config.SPool.Get().([]byte)[:]
		read, err := tcpConn.Read(buf[:])
		if err != nil {
			log.Errorf("[TUN] Failed to read from tcp conn: %v", err)
			config.SPool.Put(buf[:])
			return
		}
		if read == 0 {
			log.Warnf("[TUN] Read from tcp conn length is %d", read)
			config.SPool.Put(buf[:])
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
				log.Errorf("Failed to parse IPv4 header: %v", err)
				config.SPool.Put(buf[:])
				continue
			}
			ipProtocol = ipHeader.Protocol
			src = ipHeader.Src
			dst = ipHeader.Dst
		} else if util.IsIPv6(buf) {
			protocol = header.IPv6ProtocolNumber
			ipHeader, err := ipv6.ParseHeader(buf[:read])
			if err != nil {
				log.Errorf("Failed to parse IPv6 header: %s", err.Error())
				config.SPool.Put(buf[:])
				continue
			}
			ipProtocol = ipHeader.NextHeader
			src = ipHeader.Src
			dst = ipHeader.Dst
		} else {
			log.Debugf("[TUN-GVISOR] Unknown packet")
			config.SPool.Put(buf[:])
			continue
		}

		h.addRoute(src, conn)
		// inner ip like 223.254.0.100/102/103 connect each other
		if config.CIDR.Contains(dst) || config.CIDR6.Contains(dst) {
			log.Tracef("[TUN-RAW] Forward to TUN device, SRC: %s, DST: %s, Length: %d", src.String(), dst.String(), read)
			util.SafeWrite(h.packetChan, &datagramPacket{
				DataLength: uint16(read),
				Data:       buf[:],
			})
			continue
		}

		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			ReserveHeaderBytes: 0,
			Payload:            buffer.MakeWithData(buf[:read]),
		})
		config.SPool.Put(buf[:])
		sniffer.LogPacket("[gVISOR] ", sniffer.DirectionRecv, protocol, pkt)
		endpoint.InjectInbound(protocol, pkt)
		pkt.DecRef()
		log.Tracef("[TUN-%s] Write to Gvisor IP-Protocol: %s, SRC: %s, DST: %s, Length: %d", layers.IPProtocol(ipProtocol).String(), layers.IPProtocol(ipProtocol).String(), src.String(), dst, read)
	}
}

func (h *gvisorTCPHandler) addRoute(src net.IP, tcpConn net.Conn) {
	value, loaded := h.routeMapTCP.LoadOrStore(src.String(), tcpConn)
	if loaded {
		if tcpConn != value.(net.Conn) {
			h.routeMapTCP.Store(src.String(), tcpConn)
			log.Debugf("[TCP] Replace route map TCP: %s -> %s-%s", src, tcpConn.LocalAddr(), tcpConn.RemoteAddr())
		}
	} else {
		log.Debugf("[TCP] Add new route map TCP: %s -> %s-%s", src, tcpConn.LocalAddr(), tcpConn.RemoteAddr())
	}
}
