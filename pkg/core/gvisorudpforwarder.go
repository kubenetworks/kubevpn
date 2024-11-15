package core

import (
	"context"
	"errors"
	"io"
	"net"

	log "github.com/sirupsen/logrus"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func UDPForwarder(s *stack.Stack, ctx context.Context) func(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
	return udp.NewForwarder(s, func(request *udp.ForwarderRequest) {
		endpointID := request.ID()
		log.Debugf("[TUN-UDP] LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress %s",
			endpointID.LocalPort, endpointID.LocalAddress.String(), endpointID.RemotePort, endpointID.RemoteAddress.String(),
		)
		w := &waiter.Queue{}
		endpoint, tErr := request.CreateEndpoint(w)
		if tErr != nil {
			log.Debugf("[TUN-UDP] Failed to create endpoint: %v", tErr)
			return
		}

		// 2, dial proxy
		addr := &net.UDPAddr{
			IP:   endpointID.LocalAddress.AsSlice(),
			Port: int(endpointID.LocalPort),
		}
		remote, err := net.DialUDP("udp", nil, addr)
		if err != nil {
			log.Errorf("[TUN-UDP] Failed to connect addr %s: %v", addr.String(), err)
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
				log.Debugf("[TUN-UDP] Write length %d data to remote", written)
				errChan <- err2
			}()
			go func() {
				i := config.LPool.Get().([]byte)[:]
				defer config.LPool.Put(i[:])
				written, err2 := io.CopyBuffer(conn, remote, i)
				log.Debugf("[TUN-UDP] Read length %d data from remote", written)
				errChan <- err2
			}()
			err = <-errChan
			if err != nil && !errors.Is(err, io.EOF) {
				log.Debugf("[TUN-UDP] Disconnect: %s >-<: %s: %v", conn.LocalAddr(), remote.RemoteAddr(), err)
			}
		}()
	}).HandlePacket
}
