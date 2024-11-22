package core

import (
	"context"
	"io"
	"net"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func UDPForwarder(s *stack.Stack, ctx context.Context) func(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
	return udp.NewForwarder(s, func(request *udp.ForwarderRequest) {
		id := request.ID()
		log.Debugf("[TUN-UDP] LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress %s",
			id.LocalPort, id.LocalAddress.String(), id.RemotePort, id.RemoteAddress.String(),
		)
		src := &net.UDPAddr{
			IP:   id.RemoteAddress.AsSlice(),
			Port: int(id.RemotePort),
		}
		dst := &net.UDPAddr{
			IP:   id.LocalAddress.AsSlice(),
			Port: int(id.LocalPort),
		}

		w := &waiter.Queue{}
		endpoint, tErr := request.CreateEndpoint(w)
		if tErr != nil {
			log.Debugf("[TUN-UDP] Failed to create endpoint to dst: %s: %v", dst.String(), tErr)
			return
		}

		// dial dst
		remote, err := net.DialUDP("udp", nil, dst)
		if err != nil {
			log.Errorf("[TUN-UDP] Failed to connect dst: %s: %v", dst.String(), err)
			return
		}

		conn := gonet.NewUDPConn(w, endpoint)
		go func() {
			defer conn.Close()
			defer remote.Close()
			errChan := make(chan error, 2)
			go func() {
				buf := config.LPool.Get().([]byte)[:]
				defer config.LPool.Put(buf[:])
				written, err2 := io.CopyBuffer(remote, conn, buf)
				log.Debugf("[TUN-UDP] Write length %d data from src: %s -> dst: %s", written, src.String(), dst.String())
				errChan <- err2
			}()
			go func() {
				buf := config.LPool.Get().([]byte)[:]
				defer config.LPool.Put(buf[:])
				written, err2 := io.CopyBuffer(conn, remote, buf)
				log.Debugf("[TUN-UDP] Read length %d data from dst: %s -> src: %s", written, dst.String(), src.String())
				errChan <- err2
			}()
			err = <-errChan
			if err != nil && !errors.Is(err, io.EOF) {
				log.Debugf("[TUN-UDP] Disconnect: %s >-<: %s: %v", conn.LocalAddr(), remote.RemoteAddr(), err)
			}
		}()
	}).HandlePacket
}
