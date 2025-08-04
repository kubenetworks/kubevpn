package core

import (
	"context"
	"io"
	"net"
	"time"

	"github.com/pkg/errors"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func LocalUDPForwarder(ctx context.Context, s *stack.Stack) func(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
	return udp.NewForwarder(s, func(request *udp.ForwarderRequest) {
		id := request.ID()
		plog.G(ctx).Infof("[TUN-UDP] LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress %s",
			id.LocalPort, id.LocalAddress.String(), id.RemotePort, id.RemoteAddress.String(),
		)
		src := &net.UDPAddr{
			IP:   id.RemoteAddress.AsSlice(),
			Port: int(id.RemotePort),
		}
		var ip net.IP
		var network string
		if id.LocalAddress.To4() != (tcpip.Address{}) {
			ip = net.ParseIP("127.0.0.1")
			network = "udp4"
		} else {
			ip = net.IPv6loopback
			network = "udp6"
		}
		dst := &net.UDPAddr{
			IP:   ip,
			Port: int(id.LocalPort),
		}

		w := &waiter.Queue{}
		endpoint, tErr := request.CreateEndpoint(w)
		if tErr != nil {
			plog.G(ctx).Errorf("[TUN-UDP] Failed to create endpoint to dst: %s: %v", dst.String(), tErr)
			return
		}

		// dial dst
		remote, err1 := net.DialUDP(network, nil, dst)
		if err1 != nil {
			plog.G(ctx).Errorf("[TUN-UDP] Failed to connect dst: %s: %v", dst.String(), err1)
			return
		}

		conn := gonet.NewUDPConn(w, endpoint)
		go func() {
			defer conn.Close()
			defer remote.Close()
			errChan := make(chan error, 2)
			go func() {
				defer util.HandleCrash()
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
				plog.G(ctx).Infof("[TUN-UDP] Write length %d data from src: %s -> dst: %s", written, src, dst)
				errChan <- err
			}()
			go func() {
				defer util.HandleCrash()
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
				plog.G(ctx).Infof("[TUN-UDP] Read length %d data from dst: %s -> src: %s", written, dst, src)
				errChan <- err
			}()
			err1 = <-errChan
			if err1 != nil && !errors.Is(err1, io.EOF) {
				plog.G(ctx).Errorf("[TUN-UDP] Disconnect: %s >-<: %s: %v", conn.LocalAddr(), remote.RemoteAddr(), err1)
			}
		}()
	}).HandlePacket
}
