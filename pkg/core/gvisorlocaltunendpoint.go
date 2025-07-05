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

func readFromEndpointWriteToTun(ctx context.Context, endpoint *channel.Endpoint, out chan<- *Packet) {
	for ctx.Err() == nil {
		pktBuffer := endpoint.ReadContext(ctx)
		if pktBuffer != nil {
			sniffer.LogPacket("[gVISOR] ", sniffer.DirectionSend, pktBuffer.NetworkProtocolNumber, pktBuffer)
			data := pktBuffer.ToView().AsSlice()
			buf := config.LPool.Get().([]byte)[:]
			n := copy(buf[1:], data)
			buf[0] = 1
			out <- NewPacket(buf[:], n+1, nil, nil)
		}
	}
}

func readFromGvisorInboundWriteToEndpoint(ctx context.Context, in <-chan *Packet, endpoint *channel.Endpoint) {
	for ctx.Err() == nil {
		var packet *Packet
		select {
		case packet = <-in:
			if packet == nil {
				return
			}
		case <-ctx.Done():
			return
		}

		// Try to determine network protocol number, default zero.
		var protocol tcpip.NetworkProtocolNumber
		var ipProtocol int
		var src, dst net.IP
		// TUN interface with IFF_NO_PI enabled, thus
		// we need to determine protocol from version field
		if util.IsIPv4(packet.data[1:packet.length]) {
			protocol = header.IPv4ProtocolNumber
			ipHeader, err := ipv4.ParseHeader(packet.data[1:packet.length])
			if err != nil {
				plog.G(ctx).Errorf("Failed to parse IPv4 header: %v", err)
				config.LPool.Put(packet.data[:])
				continue
			}
			ipProtocol = ipHeader.Protocol
			src = ipHeader.Src
			dst = ipHeader.Dst
		} else if util.IsIPv6(packet.data[1:packet.length]) {
			protocol = header.IPv6ProtocolNumber
			ipHeader, err := ipv6.ParseHeader(packet.data[1:packet.length])
			if err != nil {
				plog.G(ctx).Errorf("[TCP-GVISOR] Failed to parse IPv6 header: %s", err.Error())
				config.LPool.Put(packet.data[:])
				continue
			}
			ipProtocol = ipHeader.NextHeader
			src = ipHeader.Src
			dst = ipHeader.Dst
		} else {
			plog.G(ctx).Errorf("[TCP-GVISOR] Unknown packet")
			config.LPool.Put(packet.data[:])
			continue
		}

		ipProto := layers.IPProtocol(ipProtocol)
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			ReserveHeaderBytes: 0,
			Payload:            buffer.MakeWithData(packet.data[1:packet.length]),
		})
		config.LPool.Put(packet.data[:])
		sniffer.LogPacket("[gVISOR] ", sniffer.DirectionRecv, protocol, pkt)
		endpoint.InjectInbound(protocol, pkt)
		pkt.DecRef()
		plog.G(ctx).Debugf("[TCP-GVISOR] Write to Gvisor. SRC: %s, DST: %s, Protocol: %s, Length: %d", src, dst, ipProto.String(), packet.length)
	}
}
