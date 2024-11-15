package core

import (
	"context"

	log "github.com/sirupsen/logrus"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func ICMPForwarder(s *stack.Stack, ctx context.Context) func(stack.TransportEndpointID, *stack.PacketBuffer) bool {
	return func(id stack.TransportEndpointID, buffer *stack.PacketBuffer) bool {
		log.Debugf("[TUN-ICMP] LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress %s",
			id.LocalPort, id.LocalAddress.String(), id.RemotePort, id.RemoteAddress.String(),
		)
		ctx1, cancelFunc := context.WithCancel(ctx)
		defer cancelFunc()
		ok, err := util.PingOnce(ctx1, id.RemoteAddress.String(), id.LocalAddress.String())
		if err != nil {
			log.Debugf("[TUN-ICMP] Failed to ping dst %s from src %s",
				id.LocalAddress.String(), id.RemoteAddress.String(),
			)
		}
		return ok
	}
}
