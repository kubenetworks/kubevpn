package core

import (
	"context"

	"gvisor.dev/gvisor/pkg/tcpip/stack"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func ICMPForwarder(ctx context.Context, s *stack.Stack) func(stack.TransportEndpointID, *stack.PacketBuffer) bool {
	return func(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
		defer pkt.DecRef()
		plog.G(ctx).Infof("[TUN-ICMP] LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress %s",
			id.LocalPort, id.LocalAddress.String(), id.RemotePort, id.RemoteAddress.String(),
		)
		return true
	}
}
