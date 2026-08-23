package core

import (
	"context"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// TestICMPv4EchoReply verifies the gvisor stack answers an ICMPv4 echo request with an echo
// reply. This is the ping-to-pod / heartbeat path. It regressed when gvisor was upgraded because
// SetTransportProtocolHandler is a no-op for a transport protocol that is not registered in the
// stack, and newGvisorStack registered only tcp+udp — so ICMPForwarder was never installed and no
// reply was ever produced. Registering icmp.NewProtocol4/6 fixes it; this test guards it.
func TestICMPv4EchoReply(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ep := channel.New(16, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	s := NewStack(ctx, ep)
	defer s.Destroy()

	src := tcpip.AddrFrom4([4]byte{198, 18, 0, 3})
	dst := tcpip.AddrFrom4([4]byte{10, 42, 0, 116})
	const ident, seq = 0x1234, 0x0001
	payload := []byte("pingpong")

	ep.InjectInbound(header.IPv4ProtocolNumber, buildICMPv4EchoRequest(src, dst, ident, seq, payload))

	rctx, rcancel := context.WithTimeout(ctx, 2*time.Second)
	defer rcancel()
	reply := ep.ReadContext(rctx)
	if reply == nil {
		t.Fatal("no ICMPv4 echo reply produced (regression: icmp transport protocol not registered)")
	}
	defer reply.DecRef()

	full := stack.PayloadSince(reply.NetworkHeader())
	defer full.Release()
	ip := header.IPv4(full.AsSlice())
	if ip.Protocol() != uint8(header.ICMPv4ProtocolNumber) {
		t.Fatalf("reply protocol = %d, want ICMPv4", ip.Protocol())
	}
	icmpReply := header.ICMPv4(ip.Payload())
	if got := icmpReply.Type(); got != header.ICMPv4EchoReply {
		t.Fatalf("reply type = %d, want EchoReply(%d)", got, header.ICMPv4EchoReply)
	}
	if icmpReply.Ident() != ident || icmpReply.Sequence() != seq {
		t.Errorf("reply ident/seq = %d/%d, want %d/%d", icmpReply.Ident(), icmpReply.Sequence(), ident, seq)
	}
	if ip.DestinationAddress() != src {
		t.Errorf("reply dst = %v, want the request src %v", ip.DestinationAddress(), src)
	}
}

func buildICMPv4EchoRequest(src, dst tcpip.Address, ident, seq uint16, payload []byte) *stack.PacketBuffer {
	icmpData := make([]byte, header.ICMPv4MinimumSize+len(payload))
	icmpHdr := header.ICMPv4(icmpData)
	icmpHdr.SetType(header.ICMPv4Echo)
	icmpHdr.SetCode(0)
	icmpHdr.SetIdent(ident)
	icmpHdr.SetSequence(seq)
	copy(icmpData[header.ICMPv4MinimumSize:], payload)
	icmpHdr.SetChecksum(0)
	icmpHdr.SetChecksum(header.ICMPv4Checksum(icmpHdr, 0))

	totalLen := header.IPv4MinimumSize + len(icmpData)
	ipData := make([]byte, totalLen)
	ipHdr := header.IPv4(ipData)
	ipHdr.Encode(&header.IPv4Fields{
		TotalLength: uint16(totalLen),
		TTL:         64,
		Protocol:    uint8(header.ICMPv4ProtocolNumber),
		SrcAddr:     src,
		DstAddr:     dst,
	})
	ipHdr.SetChecksum(0)
	ipHdr.SetChecksum(^ipHdr.CalculateChecksum())
	copy(ipData[header.IPv4MinimumSize:], icmpData)

	return stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(ipData)})
}
