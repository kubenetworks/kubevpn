package tunnel

import (
	"context"
	"fmt"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/sniffer"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

func readFromEndpointWriteToTun(ctx context.Context, endpoint *channel.Endpoint, out chan<- *packet, headroom int) {
	stage := diagnosticStage(ctx, "gvisor_output_enqueue", "local", -1, 0)
	defer stage.close()
	prefix := fmt.Sprintf("[gVISOR]%s ", plog.GenStr(plog.GetFields(ctx)))
	for ctx.Err() == nil {
		pkt := endpoint.ReadContext(ctx)
		if pkt != nil {
			logStackPacket(ctx, prefix, sniffer.DirectionSend, pkt.NetworkProtocolNumber, pkt)
			buf, length := copyPacketToPool(pkt, packetTypeToTUN, headroom)
			// Parse src/dst so a shared server-bound consumer (runConnPool's five-tuple hash) can
			// route this packet to a slot. Without a dst, runConnPool would treat it as a heartbeat
			// and broadcast it to every slot. Harmless for the self-to-self stack, whose output goes
			// straight to the TUN (writeToTun ignores dst).
			src, dst, _, _ := netutil.ParseIPFast(buf[tunReserve : datagramHeaderLen+length])
			// Honor cancellation so this goroutine cannot block forever on a full
			// channel after its consumer has stopped draining (e.g. reconnect/shutdown).
			packet := newPacket(buf[:], length, src, dst)
			if !diagnosticEnqueue(ctx, out, packet, stage) {
				packet.release()
				return
			}
		}
	}
}

func readFromGvisorInboundWriteToEndpoint(ctx context.Context, in <-chan *packet, endpoint *channel.Endpoint) {
	stage := diagnosticStage(ctx, "gvisor_inject", "local", -1, 0)
	defer stage.close()
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
			logStackPacket(ctx, prefix, sniffer.DirectionRecv, protocol, pkt)
			token := stage.begin()
			endpoint.InjectInbound(protocol, pkt)
			stage.finish(token, 1, len(ip), ctx.Err() == nil)
			pkt.DecRef()
		case <-ctx.Done():
			return
		}
	}
}
