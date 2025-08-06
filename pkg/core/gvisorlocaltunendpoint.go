package core

import (
	"context"

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
		pkt := endpoint.ReadContext(ctx)
		if pkt != nil {
			sniffer.LogPacket("[gVISOR] ", sniffer.DirectionSend, pkt.NetworkProtocolNumber, pkt)
			data := pkt.ToView().AsSlice()
			buf := config.LPool.Get().([]byte)[:]
			n := copy(buf[1:], data)
			buf[0] = 0
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
		// TUN interface with IFF_NO_PI enabled, thus
		// we need to determine protocol from version field
		if util.IsIPv4(packet.data[1:packet.length]) {
			protocol = header.IPv4ProtocolNumber
		} else if util.IsIPv6(packet.data[1:packet.length]) {
			protocol = header.IPv6ProtocolNumber
		} else {
			plog.G(ctx).Errorf("[TCP-GVISOR] Unknown packet")
			config.LPool.Put(packet.data[:])
			continue
		}

		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			ReserveHeaderBytes: 0,
			Payload:            buffer.MakeWithData(packet.data[1:packet.length]),
		})
		config.LPool.Put(packet.data[:])
		sniffer.LogPacket("[gVISOR] ", sniffer.DirectionRecv, protocol, pkt)
		endpoint.InjectInbound(protocol, pkt)
		pkt.DecRef()
	}
}
