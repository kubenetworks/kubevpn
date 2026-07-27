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

type gvisorLocalHandler struct {
	gvisorInbound <-chan *Packet
	outbound      chan<- *Packet
	headroom      int
	errChan       chan error
}

// handleGvisorPacket creates a local gvisor handler.
// headroom reserves bytes before the prefix in output packets for framing headers.
func handleGvisorPacket(gvisorInbound <-chan *Packet, outbound chan<- *Packet, headroom int) *gvisorLocalHandler {
	return &gvisorLocalHandler{
		gvisorInbound: gvisorInbound,
		outbound:      outbound,
		headroom:      headroom,
		errChan:       make(chan error, 1),
	}
}

// interClientStack is the shared, transport-level gvisor stack that terminates inbound
// cross-client traffic — packets a peer (client A) sends to THIS client's TUN IP — and forwards
// them to 127.0.0.1 via LocalTCPForwarder/LocalUDPForwarder.
//
// Unlike the self-to-self loopback stack (gvisorLocalHandler, fed by a Go channel), packets are
// injected DIRECTLY by each connection-pool slot's reader via InjectIP — mirroring the server's
// readFromTCPConnWriteToEndpoint (gvisor_tun_endpoint.go). This matters for throughput: the old
// design pushed inbound packets onto a bounded Go channel with drop-if-full, so under a bulk A->B
// transfer a slower-than-line-rate stack turned into silent in-order TCP segment loss on the
// receiver, forcing the sender into retransmits/RTO (see docs/50). Direct injection removes that
// intermediate queue entirely: flow control is now TCP's own receive window (a full window slows
// the peer), exactly as on the server. InjectInbound delivers synchronously and returns quickly
// (the localhost copy runs in the forwarder's own goroutines), so a busy stack never stalls a
// slot's reader — preserving heartbeat-reply liveness on the data conns.
//
// Concurrent InjectIP from all pool slots into the single shared endpoint is safe: the server
// already injects concurrently from every pool conn into one per-client stack endpoint.
type interClientStack struct {
	endpoint *channel.Endpoint
	stack    *stack.Stack
}

// newInterClientStack builds the shared inter-client stack and starts its output pump, which
// drains stack-generated packets (replies to the peer) to out (the client's tunInbound) with the
// given framing headroom. The stack and endpoint are torn down when ctx is cancelled.
func newInterClientStack(ctx context.Context, out chan<- *Packet, headroom int) *interClientStack {
	endpoint := channel.New(MaxSize, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	endpoint.LinkEPCapabilities = stack.CapabilityRXChecksumOffload
	// GVisor (software) GSO, consistent with the self-to-self and server stacks: gvisor splits
	// large segments to <=MTU internally before they reach this endpoint (not HostGSO, which would
	// push super-MTU segments a TUN cannot write). See gvisorLocalHandler.Run.
	endpoint.SupportedGSOKind = stack.GVisorGSOSupported
	s := NewLocalStack(ctx, sniffer.NewWithPrefix(endpoint, fmt.Sprintf("[gVISOR]%s ", plog.GenStr(plog.GetFields(ctx)))))
	go func() {
		defer netutil.HandleCrash()
		readFromEndpointWriteToTun(ctx, endpoint, out, headroom)
	}()
	go func() {
		<-ctx.Done()
		endpoint.Close()
		s.Destroy()
	}()
	return &interClientStack{endpoint: endpoint, stack: s}
}

// InjectIP delivers one raw IP packet (IPv4/IPv6) into the stack. It returns promptly: gvisor
// delivers synchronously up to the transport layer, where a full TCP receive buffer is handled by
// flow control (a shrinking window), not by a blind drop. Non-IP payloads are ignored.
func (ics *interClientStack) InjectIP(ip []byte) {
	var protocol tcpip.NetworkProtocolNumber
	if netutil.IsIPv4(ip) {
		protocol = header.IPv4ProtocolNumber
	} else if netutil.IsIPv6(ip) {
		protocol = header.IPv6ProtocolNumber
	} else {
		return
	}
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(ip)})
	ics.endpoint.InjectInbound(protocol, pkt)
	pkt.DecRef()
}

func (h *gvisorLocalHandler) Run(ctx context.Context) {
	endpoint := channel.New(MaxSize, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	// for support ipv6 skip checksum
	// vendor/gvisor.dev/gvisor/pkg/tcpip/stack/nic.go:763
	endpoint.LinkEPCapabilities = stack.CapabilityRXChecksumOffload
	// GVisor (software) GSO: gvisor builds large segments and splits them to <=MTU internally
	// before they reach this endpoint, so the endpoint/tunnel still sees normal packets. Kept
	// consistent with the server stack (gvisor_tcp_handler.go). Not HostGSO: these local stacks
	// (self-to-self and inter-client) feed a TCP tunnel, not a host NIC, and host-GSO would push
	// super-MTU segments onto the wire that macOS/Windows clients cannot write to their TUN.
	endpoint.SupportedGSOKind = stack.GVisorGSOSupported
	defer endpoint.Close()
	go func() {
		defer netutil.HandleCrash()
		readFromGvisorInboundWriteToEndpoint(ctx, h.gvisorInbound, endpoint)
		netutil.SafeClose(h.errChan)
	}()
	go func() {
		defer netutil.HandleCrash()
		readFromEndpointWriteToTun(ctx, endpoint, h.outbound, h.headroom)
		netutil.SafeClose(h.errChan)
	}()
	s := NewLocalStack(ctx, sniffer.NewWithPrefix(endpoint, fmt.Sprintf("[gVISOR]%s ", plog.GenStr(plog.GetFields(ctx)))))
	defer s.Destroy()
	select {
	case <-h.errChan:
		return
	case <-ctx.Done():
		return
	}
}
