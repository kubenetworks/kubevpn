package core

import (
	"context"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/sniffer"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type gvisorLocalHandler struct {
	// read from tcp conn write to gvisor inbound
	gvisorInbound <-chan *Packet
	// write to tcp conn
	gvisorOutbound chan<- *Packet

	outbound chan<- *Packet
	errChan  chan error
}

func handleGvisorPacket(gvisorInbound <-chan *Packet, outbound chan<- *Packet) *gvisorLocalHandler {
	return &gvisorLocalHandler{
		gvisorInbound: gvisorInbound,
		outbound:      outbound,
		errChan:       make(chan error, 1),
	}
}

func (h *gvisorLocalHandler) Run(ctx context.Context) {
	endpoint := channel.New(tcp.DefaultReceiveBufferSize, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	go func() {
		defer util.HandleCrash()
		readFromGvisorInboundWriteToEndpoint(ctx, h.gvisorInbound, endpoint)
		util.SafeClose(h.errChan)
	}()
	go func() {
		defer util.HandleCrash()
		readFromEndpointWriteToTun(ctx, endpoint, h.outbound)
		util.SafeClose(h.errChan)
	}()
	stack := NewLocalStack(ctx, sniffer.NewWithPrefix(endpoint, "[gVISOR] "))
	defer stack.Destroy()
	select {
	case <-h.errChan:
		return
	case <-ctx.Done():
		return
	}
}
