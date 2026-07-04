package core

import (
	"context"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func ICMPForwarder(ctx context.Context, s *stack.Stack) func(stack.TransportEndpointID, *stack.PacketBuffer) bool {
	return func(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
		defer pkt.DecRef()

		switch pkt.NetworkProtocolNumber {
		case header.IPv4ProtocolNumber:
			hdr := header.ICMPv4(pkt.TransportHeader().Slice())
			if hdr.Type() == header.ICMPv4Echo {
				go replyICMPv4Echo(ctx, s, id, hdr, pkt.Data().AsRange().ToSlice())
			}
		case header.IPv6ProtocolNumber:
			hdr := header.ICMPv6(pkt.TransportHeader().Slice())
			if hdr.Type() == header.ICMPv6EchoRequest {
				go replyICMPv6Echo(ctx, s, id, hdr, pkt.Data().AsRange().ToSlice())
			}
		}
		return true
	}
}

func replyICMPv4Echo(ctx context.Context, s *stack.Stack, id stack.TransportEndpointID, reqHdr header.ICMPv4, payload []byte) {
	defer util.HandleCrash()
	plog.G(ctx).Debugf("[Gvisor-ICMP] Echo request: %s -> %s", id.RemoteAddress, id.LocalAddress)

	replyData := make([]byte, header.ICMPv4MinimumSize+len(payload))
	replyHdr := header.ICMPv4(replyData)
	replyHdr.SetType(header.ICMPv4EchoReply)
	replyHdr.SetCode(0)
	replyHdr.SetIdent(reqHdr.Ident())
	replyHdr.SetSequence(reqHdr.Sequence())
	copy(replyHdr.Payload(), payload)
	replyHdr.SetChecksum(0)
	replyHdr.SetChecksum(header.ICMPv4Checksum(replyHdr, 0))

	route, tcpipErr := s.FindRoute(1, id.LocalAddress, id.RemoteAddress, header.IPv4ProtocolNumber, false)
	if tcpipErr != nil {
		plog.G(ctx).Errorf("[Gvisor-ICMP] Failed to find route %s -> %s: %v", id.LocalAddress, id.RemoteAddress, tcpipErr)
		return
	}
	defer route.Release()

	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		ReserveHeaderBytes: int(route.MaxHeaderLength()),
		Payload:            buffer.MakeWithData(replyData),
	})
	defer pkt.DecRef()

	if tcpipErr = route.WritePacket(stack.NetworkHeaderParams{
		Protocol: header.ICMPv4ProtocolNumber,
		TTL:      64,
		TOS:      0,
	}, pkt); tcpipErr != nil {
		plog.G(ctx).Errorf("[Gvisor-ICMP] Failed to write echo reply: %v", tcpipErr)
		return
	}
	plog.G(ctx).Debugf("[Gvisor-ICMP] Sent echo reply: %s -> %s", id.LocalAddress, id.RemoteAddress)
}

func replyICMPv6Echo(ctx context.Context, s *stack.Stack, id stack.TransportEndpointID, reqHdr header.ICMPv6, payload []byte) {
	defer util.HandleCrash()
	plog.G(ctx).Debugf("[Gvisor-ICMP] ICMPv6 echo request: %s -> %s", id.RemoteAddress, id.LocalAddress)

	replyData := make([]byte, header.ICMPv6EchoMinimumSize+len(payload))
	replyHdr := header.ICMPv6(replyData)
	replyHdr.SetType(header.ICMPv6EchoReply)
	replyHdr.SetCode(0)
	replyHdr.SetIdent(reqHdr.Ident())
	replyHdr.SetSequence(reqHdr.Sequence())
	copy(replyHdr[header.ICMPv6EchoMinimumSize:], payload)

	route, tcpipErr := s.FindRoute(1, id.LocalAddress, id.RemoteAddress, header.IPv6ProtocolNumber, false)
	if tcpipErr != nil {
		plog.G(ctx).Errorf("[Gvisor-ICMP] Failed to find route %s -> %s: %v", id.LocalAddress, id.RemoteAddress, tcpipErr)
		return
	}
	defer route.Release()

	replyHdr.SetChecksum(0)
	replyHdr.SetChecksum(header.ICMPv6Checksum(header.ICMPv6ChecksumParams{
		Header:      replyHdr,
		Src:         route.LocalAddress(),
		Dst:         route.RemoteAddress(),
		PayloadCsum: 0,
		PayloadLen:  len(payload),
	}))

	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		ReserveHeaderBytes: int(route.MaxHeaderLength()),
		Payload:            buffer.MakeWithData(replyData),
	})
	defer pkt.DecRef()

	if tcpipErr = route.WritePacket(stack.NetworkHeaderParams{
		Protocol: header.ICMPv6ProtocolNumber,
		TTL:      64,
	}, pkt); tcpipErr != nil {
		plog.G(ctx).Errorf("[Gvisor-ICMP] Failed to write ICMPv6 echo reply: %v", tcpipErr)
		return
	}
	plog.G(ctx).Debugf("[Gvisor-ICMP] Sent ICMPv6 echo reply: %s -> %s", id.LocalAddress, id.RemoteAddress)
}
