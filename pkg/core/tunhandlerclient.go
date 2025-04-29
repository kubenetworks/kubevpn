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
			err = handlePacketClient(ctx, d.tunInbound, d.tunOutbound, conn)
			if err != nil {
				plog.G(ctx).Errorf("Failed to transport data to remote %s: %v", conn.RemoteAddr(), err)
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

func handlePacketClient(ctx context.Context, tunInbound <-chan *Packet, tunOutbound chan<- *Packet, conn net.Conn) error {
	errChan := make(chan error, 2)

	go func() {
		defer util.HandleCrash()
		for packet := range tunInbound {
			_, err := conn.Write(packet.data[:packet.length])
			config.LPool.Put(packet.data[:])
			if err != nil {
				plog.G(ctx).Errorf("Failed to write packet to remote: %v", err)
				util.SafeWrite(errChan, errors.Wrap(err, "failed to write packet to remote"))
				return
			}
		}
	}()

	go func() {
		defer util.HandleCrash()
		for {
			buf := config.LPool.Get().([]byte)[:]
			n, err := conn.Read(buf[:])
			if err != nil {
				config.LPool.Put(buf[:])
				plog.G(ctx).Errorf("Failed to read packet from remote: %v", err)
				util.SafeWrite(errChan, errors.Wrap(err, fmt.Sprintf("failed to read packet from remote %s", conn.RemoteAddr())))
				return
			}
			if n == 0 {
				plog.G(ctx).Warnf("Packet length 0")
				config.LPool.Put(buf[:])
				continue
			}
			util.SafeWrite(tunOutbound, NewPacket(buf[:], n, nil, nil), func(v *Packet) {
				config.LPool.Put(v.data[:])
				plog.G(context.Background()).Errorf("Drop packet, LocalAddr: %s, Remote: %s, Length: %d", conn.LocalAddr(), conn.RemoteAddr(), v.length)
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
			plog.G(ctx).Errorf("Failed to read packet from tun device: %s", err)
			util.SafeWrite(d.errChan, err)
			config.LPool.Put(buf[:])
			return
		}

		// Try to determine network protocol number, default zero.
		var src, dst net.IP
		var protocol int
		src, dst, protocol, err = util.ParseIP(buf[:n])
		if err != nil {
			plog.G(ctx).Errorf("Unknown packet: %v", err)
			config.LPool.Put(buf[:])
			continue
		}
		plog.G(context.Background()).Debugf("SRC: %s, DST: %s, Protocol: %s, Length: %d", src, dst, layers.IPProtocol(protocol).String(), n)
		packet := NewPacket(buf[:], n, src, dst)
		f := func(v *Packet) {
			config.LPool.Put(v.data[:])
			plog.G(context.Background()).Errorf("Drop packet, SRC: %s, DST: %s, Protocol: %s, Length: %d", v.src, v.dst, layers.IPProtocol(protocol).String(), v.length)
		}
		if packet.src.Equal(packet.dst) {
			util.SafeWrite(d.tunOutbound, packet, f)
			continue
		}
		util.SafeWrite(d.tunInbound, packet, f)
	}
}

func (d *ClientDevice) writeToTun(ctx context.Context) {
	defer util.HandleCrash()
	for packet := range d.tunOutbound {
		_, err := d.tun.Write(packet.data[:packet.length])
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
