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

func (h *tunHandler) HandleClient(ctx context.Context, tun net.Conn, forwarder *Forwarder) {
	device := &ClientDevice{tunDevice{
		tun:         tun,
		tunInbound:  make(chan *Packet, MaxSize),
		tunOutbound: make(chan *Packet, MaxSize),
		errChan:     h.errChan,
	}}

	defer device.Close()
	go device.handlePacket(ctx, forwarder)
	go device.readFromTun(ctx)
	go device.writeToTun(ctx)
	go device.heartbeats(ctx)
	select {
	case <-device.errChan:
	case <-ctx.Done():
	}
}

// ClientDevice is the client-side tun device handler.
type ClientDevice struct {
	tunDevice
}

func (d *ClientDevice) handlePacket(ctx context.Context, forward *Forwarder) {
	for ctx.Err() == nil {
		func() {
			subCtx, cancel := context.WithCancel(ctx)
			defer cancel()

			defer time.Sleep(time.Second * 2)
			conn, err := forward.DialContext(subCtx)
			if err != nil {
				plog.G(ctx).Errorf("[Client] Failed to dial %s from %s: %v", forward.Addr, d.tun.LocalAddr(), err)
				return
			}
			defer conn.Close()

			errChan := make(chan error, 2)
			go readFromConn(subCtx, conn, d.tunInbound, d.tunOutbound, errChan)
			go writeToConn(subCtx, conn, d.tunInbound, errChan)

			select {
			case err = <-errChan:
				plog.G(ctx).Errorf("[Client] Transport error to %s: %v", conn.RemoteAddr(), err)
			case <-ctx.Done():
				return
			}
		}()
	}
}

func readFromConn(ctx context.Context, conn net.Conn, tunInbound chan *Packet, tunOutbound chan *Packet, errChan chan error) {
	defer util.HandleCrash()
	var gvisorInbound = make(chan *Packet, MaxSize)
	go handleGvisorPacket(gvisorInbound, tunInbound).Run(ctx)
	for ctx.Err() == nil {
		buf := config.LPool.Get().([]byte)[:]
		// Use 3x KeepAliveTime to tolerate missed heartbeat cycles.
		// Heartbeats are sent every KeepAliveTime, but the server gvisor stack
		// may not reply to ICMP echo. TCP keepalive handles NAT traversal.
		err := conn.SetReadDeadline(time.Now().Add(config.KeepAliveTime * 3))
		if err != nil {
			config.LPool.Put(buf[:])
			plog.G(ctx).Errorf("[Client] Failed to set read deadline: %v", err)
			util.SafeWrite(errChan, errors.Wrap(err, "failed to set read deadline"))
			return
		}
		n, err := conn.Read(buf[:])
		if err != nil {
			config.LPool.Put(buf[:])
			plog.G(ctx).Errorf("[Client] Failed to read from remote: %v", err)
			util.SafeWrite(errChan, errors.Wrap(err, fmt.Sprintf("failed to read packet from remote %s", conn.RemoteAddr())))
			return
		}
		if n < 1 {
			plog.G(ctx).Warnf("[Client] Received empty packet from remote")
			config.LPool.Put(buf[:])
			continue
		}
		if buf[0] == 1 {
			gvisorInbound <- NewPacket(buf[:], n, nil, nil)
		} else {
			tunOutbound <- NewPacket(buf[:], n, nil, nil)
		}
	}
}

func writeToConn(ctx context.Context, conn net.Conn, inbound <-chan *Packet, errChan chan error) {
	defer util.HandleCrash()
	for {
		select {
		case packet := <-inbound:
			if packet == nil {
				return
			}
			if err := conn.SetWriteDeadline(time.Now().Add(config.KeepAliveTime)); err != nil {
				plog.G(ctx).Errorf("[Client] Failed to set write deadline: %v", err)
				util.SafeWrite(errChan, errors.Wrap(err, "failed to set write deadline"))
				return
			}
			_, err := conn.Write(packet.data[:packet.length])
			config.LPool.Put(packet.data[:])
			if err != nil {
				plog.G(ctx).Errorf("[Client] Failed to write to remote: %v", err)
				util.SafeWrite(errChan, errors.Wrap(err, "failed to write packet to remote"))
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (d *ClientDevice) readFromTun(ctx context.Context) {
	defer util.HandleCrash()
	var gvisorInbound = make(chan *Packet, MaxSize)
	go handleGvisorPacket(gvisorInbound, d.tunOutbound).Run(ctx)
	for ctx.Err() == nil {
		buf := config.LPool.Get().([]byte)[:]
		n, err := d.tun.Read(buf[1:])
		if err != nil {
			plog.G(ctx).Errorf("[Client] Failed to read from TUN: %v", err)
			util.SafeWrite(d.errChan, err)
			config.LPool.Put(buf[:])
			return
		}
		// local client handle it with gvisor
		buf[0] = 1

		// Try to determine network protocol number, default zero.
		var src, dst net.IP
		var protocol int
		src, dst, protocol, err = util.ParseIP(buf[1 : n+1])
		if err != nil {
			plog.G(ctx).Errorf("[Client] Unknown packet, dropping: %v", err)
			config.LPool.Put(buf[:])
			continue
		}
		plog.G(ctx).Debugf("[Client] SRC: %s, DST: %s, Protocol: %s, Length: %d", src, dst, layers.IPProtocol(protocol).String(), n)
		packet := NewPacket(buf[:], n+1, src, dst)
		if packet.src.Equal(packet.dst) {
			gvisorInbound <- packet
		} else {
			d.tunInbound <- packet
		}
	}
}

func (d *ClientDevice) heartbeats(ctx context.Context) {
	tunIfi, err := util.GetTunDeviceByConn(d.tun)
	if err != nil {
		plog.G(ctx).Errorf("[Client] Failed to get tun device: %v", err)
		return
	}
	srcIPv4, srcIPv6, dockerSrcIPv4, err := util.GetTunDeviceIP(tunIfi.Name)
	if err != nil {
		plog.G(ctx).Errorf("[Client] Failed to get IP for device %s: %v", tunIfi.Name, err)
		return
	}

	ticker := time.NewTicker(config.KeepAliveTime)
	defer ticker.Stop()

	sendHeartbeat := func(payload []byte) {
		buf := config.LPool.Get().([]byte)
		n := copy(buf[1:], payload)
		buf[0] = 1
		d.tunInbound <- &Packet{data: buf, length: n + 1}
	}

	for ; ctx.Err() == nil; <-ticker.C {
		if srcIPv4 != nil {
			if icmp, e := util.GenICMPPacket(srcIPv4, config.RouterIP); e != nil {
				plog.G(ctx).Errorf("[Client] Failed to generate IPv4 heartbeat: %v", e)
			} else {
				sendHeartbeat(icmp)
			}
		}
		if srcIPv6 != nil {
			if icmp, e := util.GenICMPPacketIPv6(srcIPv6, config.RouterIP6); e != nil {
				plog.G(ctx).Errorf("[Client] Failed to generate IPv6 heartbeat: %v", e)
			} else {
				sendHeartbeat(icmp)
			}
		}
		if dockerSrcIPv4 != nil {
			_, _ = util.Ping(ctx, dockerSrcIPv4.String(), config.DockerRouterIP.String())
		}
	}
}
