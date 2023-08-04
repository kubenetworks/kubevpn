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
	remoteAddr, err := net.ResolveUDPAddr("udp", h.node.Remote)
	if err != nil {
		log.Errorf("[tun] %s: remote addr: %v", tun.LocalAddr(), err)
		return
	}

	device := &Device{
		tun:           tun,
		thread:        MaxThread,
		tunInboundRaw: make(chan *DataElem, MaxSize),
		tunInbound:    make(chan *DataElem, MaxSize),
		tunOutbound:   make(chan *DataElem, MaxSize),
		chExit:        h.chExit,
	}
	device.SetTunInboundHandler(func(tunInbound <-chan *DataElem, tunOutbound chan<- *DataElem) {
		for {
			packetConn, err := getRemotePacketConn(ctx, h.chain)
			if err != nil {
				log.Debugf("[tun] %s - %s: %s", tun.LocalAddr(), remoteAddr, err)
				time.Sleep(time.Second * 2)
				continue
			}
			err = transportTunClient(ctx, tunInbound, tunOutbound, packetConn, remoteAddr)
			if err != nil {
				log.Debugf("[tun] %s: %v", tun.LocalAddr(), err)
			}
		}
	})

	defer device.Close()
	device.Start(ctx)
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
			b := config.LPool.Get().([]byte)
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
