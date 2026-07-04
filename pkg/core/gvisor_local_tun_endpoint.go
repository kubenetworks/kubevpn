package core

import (
	"context"
	"fmt"

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

func readFromEndpointWriteToTun(ctx context.Context, endpoint *channel.Endpoint, out chan<- *Packet, headroom int) {
	prefix := fmt.Sprintf("[gVISOR]%s ", plog.GenStr(plog.GetFields(ctx)))
	for ctx.Err() == nil {
		pkt := endpoint.ReadContext(ctx)
		if pkt != nil {
			sniffer.LogPacket(prefix, sniffer.DirectionSend, pkt.NetworkProtocolNumber, pkt)
			buf, length := copyPacketToPool(pkt, 0, headroom)
			// Honor cancellation so this goroutine cannot block forever on a full
			// channel after its consumer has stopped draining (e.g. reconnect/shutdown).
			select {
			case out <- NewPacket(buf[:], length, nil, nil):
			case <-ctx.Done():
				config.LPool.Put(buf[:])
				return
			}
		}
	}
}

func readFromGvisorInboundWriteToEndpoint(ctx context.Context, in <-chan *Packet, endpoint *channel.Endpoint) {
	defer drainPacketChan(in)
	prefix := fmt.Sprintf("[gVISOR]%s ", plog.GenStr(plog.GetFields(ctx)))
	for {
		select {
		case packet := <-in:
			if packet == nil {
				return
			}
			var protocol tcpip.NetworkProtocolNumber
			ip := packet.data[tunReserve : datagramHeaderLen+packet.length]
			if util.IsIPv4(ip) {
				protocol = header.IPv4ProtocolNumber
			} else if util.IsIPv6(ip) {
				protocol = header.IPv6ProtocolNumber
			} else {
				plog.G(ctx).Errorf("[Gvisor-TCP] Unknown packet, dropping")
				config.LPool.Put(packet.data[:])
				continue
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
				Payload: buffer.MakeWithData(ip),
			})
			config.LPool.Put(packet.data[:])
			sniffer.LogPacket(prefix, sniffer.DirectionRecv, protocol, pkt)
			endpoint.InjectInbound(protocol, pkt)
			pkt.DecRef()
		case <-ctx.Done():
			return
		}
	}
}
