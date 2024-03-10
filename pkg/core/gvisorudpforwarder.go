package core

import (
	"context"
	"errors"
	"io"

	log "github.com/sirupsen/logrus"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

var GvisorUDPForwardAddr string

func UDPForwarder(s *stack.Stack) func(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
	return udp.NewForwarder(s, func(request *udp.ForwarderRequest) {
		endpointID := request.ID()
		log.Debugf("[TUN-UDP] Debug: LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress %s",
			endpointID.LocalPort, endpointID.LocalAddress.String(), endpointID.RemotePort, endpointID.RemoteAddress.String(),
		)
		w := &waiter.Queue{}
		endpoint, tErr := request.CreateEndpoint(w)
		if tErr != nil {
			log.Debugf("[TUN-UDP] Error: can not create endpoint: %v", tErr)
			return
		}

		node, err := ParseNode(GvisorUDPForwardAddr)
		if err != nil {
			log.Debugf("[TUN-UDP] Error: parse gviosr udp forward addr %s: %v", GvisorUDPForwardAddr, err)
			return
		}
		node.Client = &Client{
			Connector:   GvisorUDPOverTCPTunnelConnector(),
			Transporter: TCPTransporter(),
		}
		forwardChain := NewChain(5, node)

		ctx := context.Background()
		c, err := forwardChain.getConn(ctx)
		if err != nil {
			log.Debugf("[TUN-UDP] Error: can not get conn: %v", err)
			return
		}
		if err = WriteProxyInfo(c, endpointID); err != nil {
			log.Debugf("[TUN-UDP] Error: can not write proxy info: %v", err)
			return
		}
		remote, err := node.Client.ConnectContext(ctx, c)
		if err != nil {
			log.Debugf("[TUN-UDP] Error: can not connect: %v", err)
			return
		}
		conn := gonet.NewUDPConn(w, endpoint)
		go func() {
			defer conn.Close()
			defer remote.Close()
			errChan := make(chan error, 2)
			go func() {
				i := config.LPool.Get().([]byte)[:]
				defer config.LPool.Put(i[:])
				written, err2 := io.CopyBuffer(remote, conn, i)
				log.Debugf("[TUN-UDP] Debug: write length %d data to remote", written)
				errChan <- err2
			}()
			go func() {
				i := config.LPool.Get().([]byte)[:]
				defer config.LPool.Put(i[:])
				written, err2 := io.CopyBuffer(conn, remote, i)
				log.Debugf("[TUN-UDP] Debug: read length %d data from remote", written)
				errChan <- err2
			}()
			err = <-errChan
			if err != nil && !errors.Is(err, io.EOF) {
				log.Debugf("[TUN-UDP] Error: dsiconnect: %s >-<: %s: %v", conn.LocalAddr(), remote.RemoteAddr(), err)
			}
		}()
	}).HandlePacket
}
