package core

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/pkg/config"
)

func (h *tunHandler) HandleClient(ctx context.Context, tun net.Conn) {
	d := &Device{
		tun:           tun,
		closed:        atomic.Bool{},
		thread:        MaxThread,
		tunInboundRaw: make(chan *DataElem, MaxSize),
		tunInbound:    make(chan *DataElem, MaxSize),
		tunOutbound:   make(chan *DataElem, MaxSize),
		chExit:        h.chExit,
	}
	defer d.Close()
	d.Start()

	remoteAddr, err := net.ResolveUDPAddr("udp", h.node.Remote)
	if err != nil {
		log.Errorf("[tun] %s: remote addr: %v", tun.LocalAddr(), err)
		return
	}

	for i := 0; i < MaxThread; i++ {
		go func() {
			for {
				if ctx.Err() != nil {
					return
				}
				var packetConn net.PacketConn
				if !h.chain.IsEmpty() {
					cc, errs := h.chain.DialContext(ctx)
					if errs != nil {
						log.Debug(errs)
						time.Sleep(time.Second * 5)
						continue
					}
					var ok bool
					if packetConn, ok = cc.(net.PacketConn); !ok {
						errs = errors.New("not a packet connection")
						log.Errorf("[tun] %s - %s: %s", tun.LocalAddr(), remoteAddr, errs)
						continue
					}
				} else {
					var errs error
					var lc net.ListenConfig
					packetConn, errs = lc.ListenPacket(ctx, "udp", "")
					if errs != nil {
						log.Error(err)
						continue
					}
				}
				errs := h.transportTunCli(ctx, d, packetConn, remoteAddr)
				if errs != nil {
					log.Debugf("[tun] %s: %v", tun.LocalAddr(), errs)
				}
			}
		}()
	}

	select {
	case s := <-h.chExit:
		log.Error(s)
		return
	case <-ctx.Done():
		return
	}
}

func (h *tunHandler) transportTunCli(ctx context.Context, d *Device, conn net.PacketConn, remoteAddr net.Addr) error {
	errChan := make(chan error, 2)
	defer conn.Close()

	go func() {
		var err error
		for e := range d.tunInbound {
			if e.src.Equal(e.dst) {
				if d.closed.Load() {
					return
				}
				d.tunOutbound <- e
				continue
			}
			_, err = conn.WriteTo(e.data[:e.length], remoteAddr)
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
			n, _, err := conn.ReadFrom(b[:])
			if err != nil {
				errChan <- err
				return
			}
			if d.closed.Load() {
				return
			}
			d.tunOutbound <- &DataElem{data: b[:], length: n}
		}
	}()

	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		return nil
	}
}
