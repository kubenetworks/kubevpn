package core

import (
	"context"
	"net"
	"sync"

	log "github.com/sirupsen/logrus"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/sniffer"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type gvisorTCPHandler struct {
	// map[srcIP]net.Conn
	routeMapTCP *sync.Map
	packetChan  chan *datagramPacket
}

func GvisorTCPHandler() Handler {
	return &gvisorTCPHandler{
		routeMapTCP: RouteMapTCP,
		packetChan:  TCPPacketChan,
	}
}

func (h *gvisorTCPHandler) Handle(ctx context.Context, tcpConn net.Conn) {
	defer tcpConn.Close()
	cancel, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()
	log.Debugf("[TCP] %s -> %s", tcpConn.RemoteAddr(), tcpConn.LocalAddr())
	h.handle(cancel, tcpConn)
}

func (h *gvisorTCPHandler) handle(ctx context.Context, tcpConn net.Conn) {
	endpoint := channel.New(tcp.DefaultReceiveBufferSize, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	errChan := make(chan error, 2)
	go func() {
		h.readFromTCPConnWriteToEndpoint(ctx, tcpConn, endpoint)
		util.SafeClose(errChan)
	}()
	go func() {
		h.readFromEndpointWriteToTCPConn(ctx, tcpConn, endpoint)
		util.SafeClose(errChan)
	}()
	stack := NewStack(ctx, sniffer.NewWithPrefix(endpoint, "[gVISOR] "))
	defer stack.Destroy()
	select {
	case <-errChan:
		return
	case <-ctx.Done():
		return
	}
}

func GvisorTCPListener(addr string) (net.Listener, error) {
	log.Debugf("Gvisor TCP listening addr: %s", addr)
	laddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}
	ln, err := net.ListenTCP("tcp", laddr)
	if err != nil {
		return nil, err
	}
	return &tcpKeepAliveListener{TCPListener: ln}, nil
}
