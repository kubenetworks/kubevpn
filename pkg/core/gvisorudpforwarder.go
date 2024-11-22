package core

import (
	"context"
	"io"
	"net"
	"time"

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
		remote, err1 := net.DialUDP("udp", nil, dst)
		if err1 != nil {
			log.Errorf("[TUN-UDP] Failed to connect dst: %s: %v", dst.String(), err1)
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

				var written int
				var err error
				for {
					err = conn.SetReadDeadline(time.Now().Add(time.Second * 120))
					if err != nil {
						break
					}
					var read int
					read, _, err = conn.ReadFrom(buf[:])
					if err != nil {
						break
					}
					written += read
					err = remote.SetWriteDeadline(time.Now().Add(time.Second * 120))
					if err != nil {
						break
					}
					_, err = remote.Write(buf[:read])
					if err != nil {
						break
					}
				}
				log.Debugf("[TUN-UDP] Write length %d data from src: %s -> dst: %s", written, src.String(), dst.String())
				errChan <- err
			}()
			go func() {
				buf := config.LPool.Get().([]byte)[:]
				defer config.LPool.Put(buf[:])

				var err error
				var written int
				for {
					err = remote.SetReadDeadline(time.Now().Add(time.Second * 120))
					if err != nil {
						break
					}
					var n int
					n, _, err = remote.ReadFromUDP(buf[:])
					if err != nil {
						break
					}
					written += n
					err = conn.SetWriteDeadline(time.Now().Add(time.Second * 120))
					if err != nil {
						break
					}
					_, err = conn.Write(buf[:n])
					if err != nil {
						break
					}
				}
				log.Debugf("[TUN-UDP] Read length %d data from dst: %s -> src: %s", written, dst.String(), src.String())
				errChan <- err
			}()
			err1 = <-errChan
			if err1 != nil && !errors.Is(err1, io.EOF) {
				log.Debugf("[TUN-UDP] Disconnect: %s >-<: %s: %v", conn.LocalAddr(), remote.RemoteAddr(), err1)
			}
		}()
	}).HandlePacket
}
