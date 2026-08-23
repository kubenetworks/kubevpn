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
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

func readFromEndpointWriteToTun(ctx context.Context, endpoint *channel.Endpoint, out chan<- *Packet, headroom int) {
	prefix := fmt.Sprintf("[gVISOR]%s ", plog.GenStr(plog.GetFields(ctx)))
	for ctx.Err() == nil {
		pkt := endpoint.ReadContext(ctx)
		if pkt != nil {
			sniffer.LogPacket(prefix, sniffer.DirectionSend, pkt.NetworkProtocolNumber, pkt)
			buf, length := copyPacketToPool(pkt, packetTypeToTUN, headroom)
			// Parse src/dst so a shared server-bound consumer (runConnPool's five-tuple hash) can
			// route this packet to a slot. Without a dst, runConnPool would treat it as a heartbeat
			// and broadcast it to every slot. Harmless for the self-to-self stack, whose output goes
			// straight to the TUN (writeToTun ignores dst).
			src, dst, _, _ := netutil.ParseIPFast(buf[tunReserve : datagramHeaderLen+length])
			// Honor cancellation so this goroutine cannot block forever on a full
			// channel after its consumer has stopped draining (e.g. reconnect/shutdown).
			select {
			case out <- NewPacket(buf[:], length, src, dst):
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
			if netutil.IsIPv4(ip) {
				protocol = header.IPv4ProtocolNumber
			} else if netutil.IsIPv6(ip) {
				protocol = header.IPv6ProtocolNumber
			} else {
				plog.G(ctx).Errorf("[Gvisor-TCP] Unknown packet, dropping")
				packet.release()
				continue
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
				Payload: buffer.MakeWithData(ip),
			})
			packet.release()
			sniffer.LogPacket(prefix, sniffer.DirectionRecv, protocol, pkt)
			endpoint.InjectInbound(protocol, pkt)
			pkt.DecRef()
		case <-ctx.Done():
			return
		}
	}
}
