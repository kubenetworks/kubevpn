package core

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/pkg/errors"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (h *tunHandler) HandleClient(ctx context.Context, tun net.Conn, remoteAddr *net.UDPAddr) {
	device := &ClientDevice{
		tun:         tun,
		tunInbound:  make(chan *Packet, MaxSize),
		tunOutbound: make(chan *Packet, MaxSize),
		errChan:     h.errChan,
	}

	defer device.Close()
	go device.forwardPacketToRemote(ctx, remoteAddr, h.forward)
	go device.readFromTun(ctx)
	go device.writeToTun(ctx)
	go heartbeats(ctx, device.tun)
	select {
	case <-device.errChan:
	case <-ctx.Done():
	}
}

type ClientDevice struct {
	tun         net.Conn
	tunInbound  chan *Packet
	tunOutbound chan *Packet
	errChan     chan error

	remote  *net.UDPAddr
	forward *Forwarder
}

func (d *ClientDevice) forwardPacketToRemote(ctx context.Context, remoteAddr *net.UDPAddr, forward *Forwarder) {
	for ctx.Err() == nil {
		func() {
			packetConn, err := getRemotePacketConn(ctx, forward)
			if err != nil {
				plog.G(ctx).Errorf("[TUN-CLIENT] Failed to get remote conn from %s -> %s: %s", d.tun.LocalAddr(), remoteAddr, err)
				time.Sleep(time.Second * 1)
				return
			}
			err = transportTunPacketClient(ctx, d.tunInbound, d.tunOutbound, packetConn, remoteAddr)
			if err != nil {
				plog.G(ctx).Errorf("[TUN-CLIENT] Failed to transport data to remote %s: %v", remoteAddr, err)
			}
		}()
	}
}

func getRemotePacketConn(ctx context.Context, forwarder *Forwarder) (packetConn net.PacketConn, err error) {
	defer func() {
		if err != nil && packetConn != nil {
			_ = packetConn.Close()
		}
	}()
	if !forwarder.IsEmpty() {
		var conn net.Conn
		conn, err = forwarder.DialContext(ctx)
		if err != nil {
			return
		}
		var ok bool
		if packetConn, ok = conn.(net.PacketConn); !ok {
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

func transportTunPacketClient(ctx context.Context, tunInbound <-chan *Packet, tunOutbound chan<- *Packet, packetConn net.PacketConn, remoteAddr net.Addr) error {
	errChan := make(chan error, 2)
	defer packetConn.Close()

	go func() {
		defer util.HandleCrash()
		for packet := range tunInbound {
			if packet.src.Equal(packet.dst) {
				util.SafeWrite(tunOutbound, packet, func(v *Packet) {
					config.LPool.Put(v.data[:])
					plog.G(context.Background()).Errorf("Drop packet, SRC: %s, DST: %s, Length: %d", v.src, v.dst, v.length)
				})
				continue
			}
			_, err := packetConn.WriteTo(packet.data[:packet.length], remoteAddr)
			config.LPool.Put(packet.data[:])
			if err != nil {
				util.SafeWrite(errChan, errors.Wrap(err, fmt.Sprintf("failed to write packet to remote %s", remoteAddr)))
				return
			}
		}
	}()

	go func() {
		defer util.HandleCrash()
		for {
			buf := config.LPool.Get().([]byte)[:]
			n, _, err := packetConn.ReadFrom(buf[:])
			if err != nil {
				config.LPool.Put(buf[:])
				util.SafeWrite(errChan, errors.Wrap(err, fmt.Sprintf("failed to read packet from remote %s", remoteAddr)))
				return
			}
			util.SafeWrite(tunOutbound, &Packet{data: buf[:], length: n}, func(v *Packet) {
				config.LPool.Put(v.data[:])
			})
		}
	}()

	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		return nil
	}
}

func (d *ClientDevice) readFromTun(ctx context.Context) {
	defer util.HandleCrash()
	for {
		buf := config.LPool.Get().([]byte)[:]
		n, err := d.tun.Read(buf[:])
		if err != nil {
			plog.G(ctx).Errorf("[TUN-CLIENT] Failed to read packet from tun device: %s", err)
			util.SafeWrite(d.errChan, err)
			config.LPool.Put(buf[:])
			return
		}

		// Try to determine network protocol number, default zero.
		var src, dst net.IP
		src, dst, err = util.ParseIP(buf[:n])
		if err != nil {
			plog.G(ctx).Errorf("[TUN-CLIENT] Unknown packet: %v", err)
			config.LPool.Put(buf[:])
			continue
		}
		plog.G(context.Background()).Debugf("[TUN-CLIENT] SRC: %s, DST: %s, Length: %d", src.String(), dst, n)
		util.SafeWrite(d.tunInbound, NewPacket(buf[:], n, src, dst), func(v *Packet) {
			config.LPool.Put(v.data[:])
			plog.G(context.Background()).Errorf("Drop packet, SRC: %s, DST: %s, Length: %d", v.src, v.dst, v.length)
		})
	}
}

func (d *ClientDevice) writeToTun(ctx context.Context) {
	defer util.HandleCrash()
	for e := range d.tunOutbound {
		_, err := d.tun.Write(e.data[:e.length])
		config.LPool.Put(e.data[:])
		if err != nil {
			plog.G(ctx).Errorf("[TUN-CLIENT] Failed to write packet to tun device: %v", err)
			util.SafeWrite(d.errChan, err)
			return
		}
	}
}

func (d *ClientDevice) Close() {
	d.tun.Close()
	util.SafeClose(d.tunInbound)
	util.SafeClose(d.tunOutbound)
}

func heartbeats(ctx context.Context, tun net.Conn) {
	tunIfi, err := util.GetTunDeviceByConn(tun)
	if err != nil {
		plog.G(ctx).Errorf("Failed to get tun device: %v", err)
		return
	}
	srcIPv4, srcIPv6, dockerSrcIPv4, err := util.GetTunDeviceIP(tunIfi.Name)
	if err != nil {
		plog.G(ctx).Errorf("Failed to get tun device %s IP: %v", tunIfi.Name, err)
		return
	}

	ticker := time.NewTicker(time.Second * 60)
	defer ticker.Stop()

	for ; true; <-ticker.C {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if srcIPv4 != nil {
			util.Ping(ctx, srcIPv4.String(), config.RouterIP.String())
		}
		if srcIPv6 != nil {
			util.Ping(ctx, srcIPv6.String(), config.RouterIP6.String())
		}
		if dockerSrcIPv4 != nil {
			util.Ping(ctx, dockerSrcIPv4.String(), config.DockerRouterIP.String())
		}
	}
}
