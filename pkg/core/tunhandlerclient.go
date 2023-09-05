package core

import (
	"context"
	"errors"
	"net"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/pkg/config"
)

func (h *tunHandler) HandleClient(ctx context.Context, tun net.Conn) {
	defer tun.Close()
	remoteAddr, err := net.ResolveUDPAddr("udp", h.node.Remote)
	if err != nil {
		log.Errorf("[tun] %s: remote addr: %v", tun.LocalAddr(), err)
		return
	}
	in := make(chan *DataElem, MaxSize)
	out := make(chan *DataElem, MaxSize)
	endpoint := NewTunEndpoint(ctx, tun, uint32(config.DefaultMTU), in, out)
	stack := NewStack(ctx, endpoint)
	go stack.Wait()

	d := &ClientDevice{
		tunInbound:  in,
		tunOutbound: out,
		chExit:      h.chExit,
	}
	d.SetTunInboundHandler(func(tunInbound <-chan *DataElem, tunOutbound chan<- *DataElem) {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			packetConn, err := getRemotePacketConn(ctx, h.chain)
			if err != nil {
				log.Debugf("[tun-client] %s - %s: %s", tun.LocalAddr(), remoteAddr, err)
				time.Sleep(time.Second * 2)
				continue
			}
			err = transportTunClient(ctx, tunInbound, tunOutbound, packetConn, remoteAddr)
			if err != nil {
				log.Debugf("[tun-client] %s: %v", tun.LocalAddr(), err)
			}
		}
	})

	d.Start(ctx)
}

func getRemotePacketConn(ctx context.Context, chain *Chain) (net.PacketConn, error) {
	var packetConn net.PacketConn
	if !chain.IsEmpty() {
		cc, err := chain.DialContext(ctx)
		if err != nil {
			return nil, err
		}
		var ok bool
		if packetConn, ok = cc.(net.PacketConn); !ok {
			return nil, errors.New("not a packet connection")
		}
	} else {
		var err error
		var lc net.ListenConfig
		packetConn, err = lc.ListenPacket(ctx, "udp", "")
		if err != nil {
			return nil, err
		}
	}
	return packetConn, nil
}

func transportTunClient(ctx context.Context, tunInbound <-chan *DataElem, tunOutbound chan<- *DataElem, packetConn net.PacketConn, remoteAddr net.Addr) error {
	errChan := make(chan error, 2)
	defer packetConn.Close()

	go func() {
		for e := range tunInbound {
			if e.src.Equal(e.dst) {
				tunOutbound <- e
				continue
			}
			_, err := packetConn.WriteTo(e.data[:e.length], remoteAddr)
			config.LPool.Put(e.data[:])
			if err != nil {
				errChan <- err
				return
			}
		}
	}()

	go func() {
		for {
			b := config.LPool.Get().([]byte)[:]
			n, _, err := packetConn.ReadFrom(b[:])
			if err != nil {
				errChan <- err
				return
			}
			tunOutbound <- &DataElem{data: b[:], length: n}
		}
	}()

	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		return nil
	}
}

type ClientDevice struct {
	tunInbound  chan *DataElem
	tunOutbound chan *DataElem
	// your main logic
	tunInboundHandler func(tunInbound <-chan *DataElem, tunOutbound chan<- *DataElem)
	chExit            chan error
}

func (d *ClientDevice) Start(ctx context.Context) {
	go d.tunInboundHandler(d.tunInbound, d.tunOutbound)
	go heartbeats(d.tunInbound)

	select {
	case err := <-d.chExit:
		log.Error(err)
		return
	case <-ctx.Done():
		return
	}
}

func (d *ClientDevice) SetTunInboundHandler(handler func(tunInbound <-chan *DataElem, tunOutbound chan<- *DataElem)) {
	d.tunInboundHandler = handler
}
