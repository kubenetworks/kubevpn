package core

import (
	"context"
	"fmt"

	"gvisor.dev/gvisor/pkg/tcpip"
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
