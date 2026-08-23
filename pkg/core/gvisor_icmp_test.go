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

// injectEchoAndReadReply injects req into ep and then DecRefs it, faithfully mirroring the
// production data-plane sequence in readFromTCPConnWriteToEndpoint / readFromGvisorInboundWriteToEndpoint
// (create packet -> InjectInbound -> caller DecRef). It fails the test if that caller-side DecRef
// panics — which is exactly the double-DecRef regression this file guards: ICMPForwarder used to
// DecRef the packet it did not own, driving the refcount to 0 before the injector's DecRef ran and
// panicking with "Decrementing non-positive ref count". The reply (if any) is returned; the caller
// owns it and must DecRef it.
func injectEchoAndReadReply(t *testing.T, ctx context.Context, ep *channel.Endpoint, proto tcpip.NetworkProtocolNumber, req *stack.PacketBuffer) *stack.PacketBuffer {
	t.Helper()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("caller-side DecRef panicked (double DecRef in ICMPForwarder?): %v", r)
			}
		}()
		// InjectInbound is synchronous for a channel.Endpoint: the stack delivers the packet to
		// ICMPForwarder and enqueues any echo reply before returning, so the reply is already
		// readable below. The injector owns req and must be the SOLE DecRef.
		ep.InjectInbound(proto, req)
		req.DecRef()
	}()

	rctx, rcancel := context.WithTimeout(ctx, 2*time.Second)
	defer rcancel()
	return ep.ReadContext(rctx)
}

// TestICMPv4EchoReply verifies the gvisor stack answers an ICMPv4 echo request with an echo
// reply. This is the ping-to-pod / heartbeat path. It regressed when gvisor was upgraded because
// SetTransportProtocolHandler is a no-op for a transport protocol that is not registered in the
// stack, and newGvisorStack registered only tcp+udp — so ICMPForwarder was never installed and no
// reply was ever produced. Registering icmp.NewProtocol4/6 fixes it; this test guards it.
//
// It also guards the packet-refcount contract: the injected request is DecRef'd after InjectInbound
// (see injectEchoAndReadReply), so if ICMPForwarder ever DecRefs the packet it does not own, this
// test panics.
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

	req := buildICMPv4EchoRequest(src, dst, ident, seq, payload)
	reply := injectEchoAndReadReply(t, ctx, ep, header.IPv4ProtocolNumber, req)
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

// TestICMPv4EchoReply_LocalStack guards the SECOND trigger path of the same refcount bug:
// readFromGvisorInboundWriteToEndpoint (pkg/core/gvisor_local_tun_endpoint.go) injects into a
// NewLocalStack the exact same way (NewPacketBuffer -> InjectInbound -> DecRef). Both NewStack and
// NewLocalStack register the same ICMPForwarder via newGvisorStack, so an ICMP echo on the local /
// reverse path double-DecRefs identically. This test would panic before the ICMPForwarder fix.
func TestICMPv4EchoReply_LocalStack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ep := channel.New(16, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	s := NewLocalStack(ctx, ep)
	defer s.Destroy()

	src := tcpip.AddrFrom4([4]byte{198, 18, 0, 3})
	dst := tcpip.AddrFrom4([4]byte{10, 42, 0, 116})
	const ident, seq = 0x2233, 0x0007
	payload := []byte("localping")

	req := buildICMPv4EchoRequest(src, dst, ident, seq, payload)
	reply := injectEchoAndReadReply(t, ctx, ep, header.IPv4ProtocolNumber, req)
	if reply == nil {
		t.Fatal("no ICMPv4 echo reply produced on local stack")
	}
	defer reply.DecRef()

	full := stack.PayloadSince(reply.NetworkHeader())
	defer full.Release()
	ip := header.IPv4(full.AsSlice())
	icmpReply := header.ICMPv4(ip.Payload())
	if got := icmpReply.Type(); got != header.ICMPv4EchoReply {
		t.Fatalf("reply type = %d, want EchoReply(%d)", got, header.ICMPv4EchoReply)
	}
	if icmpReply.Ident() != ident || icmpReply.Sequence() != seq {
		t.Errorf("reply ident/seq = %d/%d, want %d/%d", icmpReply.Ident(), icmpReply.Sequence(), ident, seq)
	}
}

// TestICMPv6EchoReply verifies the ICMPv6 echo path: reply produced, ident/seq/dst correct, the
// injected request refcount contract honored (no double DecRef), and — critically — the reply
// carries a VALID ICMPv6 checksum (the checksum covers a pseudo-header whose length must not be
// double-counted).
func TestICMPv6EchoReply(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ep := channel.New(16, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	s := NewStack(ctx, ep)
	defer s.Destroy()

	src := tcpip.AddrFrom16([16]byte{0xfd, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x03})
	dst := tcpip.AddrFrom16([16]byte{0xfd, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x74})
	const ident, seq = 0x4321, 0x0002
	payload := []byte("pingpong")

	req := buildICMPv6EchoRequest(src, dst, ident, seq, payload)
	reply := injectEchoAndReadReply(t, ctx, ep, header.IPv6ProtocolNumber, req)
	if reply == nil {
		t.Fatal("no ICMPv6 echo reply produced")
	}
	defer reply.DecRef()

	full := stack.PayloadSince(reply.NetworkHeader())
	defer full.Release()
	ip := header.IPv6(full.AsSlice())
	if ip.TransportProtocol() != header.ICMPv6ProtocolNumber {
		t.Fatalf("reply next header = %d, want ICMPv6", ip.TransportProtocol())
	}
	icmpReply := header.ICMPv6(ip.Payload())
	if got := icmpReply.Type(); got != header.ICMPv6EchoReply {
		t.Fatalf("reply type = %d, want EchoReply(%d)", got, header.ICMPv6EchoReply)
	}
	if icmpReply.Ident() != ident || icmpReply.Sequence() != seq {
		t.Errorf("reply ident/seq = %d/%d, want %d/%d", icmpReply.Ident(), icmpReply.Sequence(), ident, seq)
	}
	if ip.DestinationAddress() != src {
		t.Errorf("reply dst = %v, want the request src %v", ip.DestinationAddress(), src)
	}
	// Recompute the checksum over the reply (the helper skips the embedded checksum field):
	// a valid packet is one where the stored checksum equals the recomputed value.
	wantCsum := header.ICMPv6Checksum(header.ICMPv6ChecksumParams{
		Header: icmpReply,
		Src:    ip.SourceAddress(),
		Dst:    ip.DestinationAddress(),
	})
	if got := icmpReply.Checksum(); got != wantCsum {
		t.Errorf("reply ICMPv6 checksum = %#x, want %#x (pseudo-header length double-counted?)", got, wantCsum)
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

func buildICMPv6EchoRequest(src, dst tcpip.Address, ident, seq uint16, payload []byte) *stack.PacketBuffer {
	icmpData := make([]byte, header.ICMPv6EchoMinimumSize+len(payload))
	icmpHdr := header.ICMPv6(icmpData)
	icmpHdr.SetType(header.ICMPv6EchoRequest)
	icmpHdr.SetCode(0)
	icmpHdr.SetIdent(ident)
	icmpHdr.SetSequence(seq)
	copy(icmpData[header.ICMPv6EchoMinimumSize:], payload)
	icmpHdr.SetChecksum(0)
	// Header already includes the payload, so PayloadLen stays 0: ICMPv6Checksum uses
	// len(Header)+PayloadLen as the pseudo-header length, and double-counting it corrupts the sum.
	icmpHdr.SetChecksum(header.ICMPv6Checksum(header.ICMPv6ChecksumParams{
		Header: icmpHdr,
		Src:    src,
		Dst:    dst,
	}))

	totalLen := header.IPv6MinimumSize + len(icmpData)
	ipData := make([]byte, totalLen)
	ipHdr := header.IPv6(ipData)
	ipHdr.Encode(&header.IPv6Fields{
		PayloadLength:     uint16(len(icmpData)),
		TransportProtocol: header.ICMPv6ProtocolNumber,
		HopLimit:          64,
		SrcAddr:           src,
		DstAddr:           dst,
	})
	copy(ipData[header.IPv6MinimumSize:], icmpData)

	return stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(ipData)})
}
