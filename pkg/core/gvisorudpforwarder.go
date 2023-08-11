package core

import (
	"context"
	"io"
	"net"

	log "github.com/sirupsen/logrus"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

func UDPForwarder(s *stack.Stack) func(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
	return udp.NewForwarder(s, func(request *udp.ForwarderRequest) {
		endpointID := request.ID()
		log.Infof("[TUN-UDP] Info: LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress %s",
			endpointID.LocalPort, endpointID.LocalAddress.String(), endpointID.RemotePort, endpointID.RemoteAddress.String(),
		)
		w := &waiter.Queue{}
		endpoint, t := request.CreateEndpoint(w)
		if t != nil {
			log.Warningln(t)
			return
		}
		c, err2 := net.Dial("tcp", "127.0.0.1:10802")
		if err2 != nil {
			log.Error(err2)
		}
		if err := WriteProxyInfo(c, endpointID); err != nil {
			log.Warningln(err)
			return
		}
		ctx := context.Background()
		dial, err := GvisorUDPOverTCPTunnelConnector(endpointID).ConnectContext(ctx, c)
		if err != nil {
			log.Warningln(err)
			return
		}
		conn := gonet.NewUDPConn(s, w, endpoint)
		go func() {
			go io.Copy(dial, conn)
			io.Copy(conn, dial)
		}()
	}).HandlePacket
}
