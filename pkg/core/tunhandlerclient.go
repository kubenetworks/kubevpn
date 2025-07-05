package core

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/google/gopacket/layers"
	"github.com/pkg/errors"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (h *tunHandler) HandleClient(ctx context.Context, tun net.Conn) {
	device := &ClientDevice{
		tun:         tun,
		tunInbound:  make(chan *Packet, MaxSize),
		tunOutbound: make(chan *Packet, MaxSize),
		errChan:     h.errChan,
	}

	defer device.Close()
	go device.handlePacket(ctx, h.forward)
	go device.readFromTun(ctx)
	go device.writeToTun(ctx)
	go device.heartbeats(ctx)
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
}

func (d *ClientDevice) handlePacket(ctx context.Context, forward *Forwarder) {
	for ctx.Err() == nil {
		func() {
			defer time.Sleep(time.Second * 2)
			conn, err := forwardConn(ctx, forward)
			if err != nil {
				plog.G(ctx).Errorf("Failed to get remote conn from %s -> %s: %s", d.tun.LocalAddr(), forward.node.Remote, err)
				return
			}
			defer conn.Close()

			errChan := make(chan error, 2)
			go readFromConn(ctx, conn, d.tunInbound, d.tunOutbound, errChan)
			go writeToConn(ctx, conn, d.tunInbound, errChan)

			select {
			case err = <-errChan:
				plog.G(ctx).Errorf("Failed to transport data to remote %s: %v", conn.RemoteAddr(), err)
			case <-ctx.Done():
				return
			}
		}()
	}
}

func forwardConn(ctx context.Context, forwarder *Forwarder) (net.Conn, error) {
	conn, err := forwarder.DialContext(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to dial forwarder")
	}
	return conn, nil
}

func readFromConn(ctx context.Context, conn net.Conn, tunInbound chan *Packet, tunOutbound chan *Packet, errChan chan error) {
	defer util.HandleCrash()
	var gvisorInbound = make(chan *Packet, MaxSize)
	go handleGvisorPacket(gvisorInbound, tunInbound).Run(ctx)
	for {
		buf := config.LPool.Get().([]byte)[:]
		err := conn.SetReadDeadline(time.Now().Add(config.KeepAliveTime))
		if err != nil {
			config.LPool.Put(buf[:])
			plog.G(ctx).Errorf("Failed to set read deadline: %v", err)
			util.SafeWrite(errChan, errors.Wrap(err, "failed to set read deadline"))
			return
		}
		n, err := conn.Read(buf[:])
		if err != nil {
			config.LPool.Put(buf[:])
			plog.G(ctx).Errorf("Failed to read packet from remote: %v", err)
			util.SafeWrite(errChan, errors.Wrap(err, fmt.Sprintf("failed to read packet from remote %s", conn.RemoteAddr())))
			return
		}
		if n < 1 {
			plog.G(ctx).Warnf("Packet length 0")
			config.LPool.Put(buf[:])
			continue
		}
		if buf[0] == 1 {
			util.SafeWrite(gvisorInbound, NewPacket(buf[:], n, nil, nil), func(v *Packet) {
				config.LPool.Put(v.data[:])
				plog.G(context.Background()).Errorf("Drop packet, LocalAddr: %s, Remote: %s, Length: %d", conn.LocalAddr(), conn.RemoteAddr(), v.length)
			})
		} else {
			util.SafeWrite(tunOutbound, NewPacket(buf[:], n, nil, nil), func(v *Packet) {
				config.LPool.Put(v.data[:])
				plog.G(context.Background()).Errorf("Drop packet, LocalAddr: %s, Remote: %s, Length: %d", conn.LocalAddr(), conn.RemoteAddr(), v.length)
			})
		}
	}
}

func writeToConn(ctx context.Context, conn net.Conn, inbound <-chan *Packet, errChan chan error) {
	defer util.HandleCrash()
	for packet := range inbound {
		err := conn.SetWriteDeadline(time.Now().Add(config.KeepAliveTime))
		if err != nil {
			plog.G(ctx).Errorf("Failed to set write deadline: %v", err)
			util.SafeWrite(errChan, errors.Wrap(err, "failed to set write deadline"))
			return
		}
		_, err = conn.Write(packet.data[:packet.length])
		config.LPool.Put(packet.data[:])
		if err != nil {
			plog.G(ctx).Errorf("Failed to write packet to remote: %v", err)
			util.SafeWrite(errChan, errors.Wrap(err, "failed to write packet to remote"))
			return
		}
	}
}

func (d *ClientDevice) readFromTun(ctx context.Context) {
	defer util.HandleCrash()
	var gvisorInbound = make(chan *Packet, MaxSize)
	go handleGvisorPacket(gvisorInbound, d.tunOutbound).Run(ctx)
	for {
		buf := config.LPool.Get().([]byte)[:]
		n, err := d.tun.Read(buf[1:])
		if err != nil {
			plog.G(ctx).Errorf("Failed to read packet from tun device: %s", err)
			util.SafeWrite(d.errChan, err)
			config.LPool.Put(buf[:])
			return
		}
		buf[0] = 1

		// Try to determine network protocol number, default zero.
		var src, dst net.IP
		var protocol int
		src, dst, protocol, err = util.ParseIP(buf[1 : n+1])
		if err != nil {
			plog.G(ctx).Errorf("Unknown packet: %v", err)
			config.LPool.Put(buf[:])
			continue
		}
		plog.G(context.Background()).Debugf("SRC: %s, DST: %s, Protocol: %s, Length: %d", src, dst, layers.IPProtocol(protocol).String(), n)
		packet := NewPacket(buf[:], n+1, src, dst)
		f := func(v *Packet) {
			config.LPool.Put(v.data[:])
			plog.G(context.Background()).Errorf("Drop packet, SRC: %s, DST: %s, Protocol: %s, Length: %d", v.src, v.dst, layers.IPProtocol(protocol).String(), v.length)
		}
		if packet.src.Equal(packet.dst) {
			// local client handle it with gvisor
			packet.data[0] = 1
			util.SafeWrite(gvisorInbound, packet, f)
		} else {
			util.SafeWrite(d.tunInbound, packet, f)
		}
	}
}

func (d *ClientDevice) writeToTun(ctx context.Context) {
	defer util.HandleCrash()
	for packet := range d.tunOutbound {
		_, err := d.tun.Write(packet.data[1:packet.length])
		config.LPool.Put(packet.data[:])
		if err != nil {
			plog.G(ctx).Errorf("Failed to write packet to tun device: %v", err)
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

func (d *ClientDevice) heartbeats(ctx context.Context) {
	tunIfi, err := util.GetTunDeviceByConn(d.tun)
	if err != nil {
		plog.G(ctx).Errorf("Failed to get tun device: %v", err)
		return
	}
	srcIPv4, srcIPv6, dockerSrcIPv4, err := util.GetTunDeviceIP(tunIfi.Name)
	if err != nil {
		plog.G(ctx).Errorf("Failed to get tun device %s IP: %v", tunIfi.Name, err)
		return
	}

	ticker := time.NewTicker(config.KeepAliveTime)
	defer ticker.Stop()

	for ; ctx.Err() == nil; <-ticker.C {
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
