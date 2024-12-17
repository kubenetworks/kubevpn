package core

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (h *tunHandler) HandleClient(ctx context.Context, tun net.Conn) {
	defer tun.Close()
	remoteAddr, err := net.ResolveUDPAddr("udp", h.node.Remote)
	if err != nil {
		log.Errorf("[TUN-CLIENT] Failed to resolve udp addr %s: %v", h.node.Remote, err)
		return
	}
	in := make(chan *DataElem, MaxSize)
	out := make(chan *DataElem, MaxSize)
	defer util.SafeClose(in)
	defer util.SafeClose(out)

	d := &ClientDevice{
		tun:         tun,
		tunInbound:  in,
		tunOutbound: out,
		chExit:      h.chExit,
	}
	d.SetTunInboundHandler(func(tunInbound <-chan *DataElem, tunOutbound chan<- *DataElem) {
		for ctx.Err() == nil {
			packetConn, err := getRemotePacketConn(ctx, h.chain)
			if err != nil {
				log.Debugf("[TUN-CLIENT] Failed to get remote conn from %s -> %s: %s", tun.LocalAddr(), remoteAddr, err)
				time.Sleep(time.Millisecond * 200)
				continue
			}
			err = transportTunClient(ctx, tunInbound, tunOutbound, packetConn, remoteAddr)
			if err != nil {
				log.Debugf("[TUN-CLIENT] %s: %v", tun.LocalAddr(), err)
			}
		}
	})

	d.Start(ctx)
}

func getRemotePacketConn(ctx context.Context, chain *Chain) (packetConn net.PacketConn, err error) {
	defer func() {
		if err != nil && packetConn != nil {
			_ = packetConn.Close()
		}
	}()
	if !chain.IsEmpty() {
		var cc net.Conn
		cc, err = chain.DialContext(ctx)
		if err != nil {
			return
		}
		var ok bool
		if packetConn, ok = cc.(net.PacketConn); !ok {
			err = errors.New("not a packet connection")
			return
		}
	} else {
		var lc net.ListenConfig
		packetConn, err = lc.ListenPacket(ctx, "udp", "")
		if err != nil {
			return
		}
	}
	return
}

func transportTunClient(ctx context.Context, tunInbound <-chan *DataElem, tunOutbound chan<- *DataElem, packetConn net.PacketConn, remoteAddr net.Addr) error {
	errChan := make(chan error, 2)
	defer packetConn.Close()

	go func() {
		for e := range tunInbound {
			if e.src.Equal(e.dst) {
				util.SafeWrite(tunOutbound, e)
				continue
			}
			_, err := packetConn.WriteTo(e.data[:e.length], remoteAddr)
			config.LPool.Put(e.data[:])
			if err != nil {
				util.SafeWrite(errChan, errors.Wrap(err, fmt.Sprintf("failed to write packet to remote %s", remoteAddr)))
				return
			}
		}
	}()

	go func() {
		for {
			buf := config.LPool.Get().([]byte)[:]
			n, _, err := packetConn.ReadFrom(buf[:])
			if err != nil {
				config.LPool.Put(buf[:])
				util.SafeWrite(errChan, errors.Wrap(err, fmt.Sprintf("failed to read packet from remote %s", remoteAddr)))
				return
			}
			util.SafeWrite(tunOutbound, &DataElem{data: buf[:], length: n})
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
	tun         net.Conn
	tunInbound  chan *DataElem
	tunOutbound chan *DataElem
	// your main logic
	tunInboundHandler func(tunInbound <-chan *DataElem, tunOutbound chan<- *DataElem)
	chExit            chan error
}

func (d *ClientDevice) Start(ctx context.Context) {
	go d.tunInboundHandler(d.tunInbound, d.tunOutbound)
	go heartbeats(ctx, d.tun)
	go d.readFromTun()
	go d.writeToTun()

	select {
	case err := <-d.chExit:
		log.Errorf("[TUN-CLIENT]: %v", err)
		return
	case <-ctx.Done():
		return
	}
}

func (d *ClientDevice) SetTunInboundHandler(handler func(tunInbound <-chan *DataElem, tunOutbound chan<- *DataElem)) {
	d.tunInboundHandler = handler
}

func (d *ClientDevice) readFromTun() {
	for {
		buf := config.LPool.Get().([]byte)[:]
		n, err := d.tun.Read(buf[:])
		if err != nil {
			util.SafeWrite(d.chExit, err)
			config.LPool.Put(buf[:])
			return
		}
		if n == 0 {
			config.LPool.Put(buf[:])
			continue
		}

		// Try to determine network protocol number, default zero.
		var src, dst net.IP
		src, dst, err = util.ParseIP(buf[:n])
		if err != nil {
			log.Debugf("[TUN-GVISOR] Unknown packet: %v", err)
			config.LPool.Put(buf[:])
			continue
		}
		log.Tracef("[TUN-RAW] SRC: %s, DST: %s, Length: %d", src.String(), dst, n)
		util.SafeWrite(d.tunInbound, NewDataElem(buf[:], n, src, dst))
	}
}

func (d *ClientDevice) writeToTun() {
	for e := range d.tunOutbound {
		_, err := d.tun.Write(e.data[:e.length])
		config.LPool.Put(e.data[:])
		if err != nil {
			util.SafeWrite(d.chExit, err)
			return
		}
	}
}
