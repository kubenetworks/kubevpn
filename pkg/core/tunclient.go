package core

import (
	"context"
	"encoding/binary"
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
	device := &ClientDevice{
		tunDevice: tunDevice{
			tun:         tun,
			tunInbound:  make(chan *Packet, MaxSize),
			tunOutbound: make(chan *Packet, MaxSize),
			errChan:     h.errChan,
		},
		reconnected: make(chan struct{}, 1),
	}

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
	// reconnected signals the heartbeat goroutine to send an immediate heartbeat
	// after a new connection is established, so the server can register the route
	// without waiting for the next ticker cycle.
	reconnected chan struct{}
}

func (d *ClientDevice) handlePacket(ctx context.Context, forward *Forwarder) {
	for ctx.Err() == nil {
		func() {
			subCtx, cancel := context.WithCancel(ctx)
			defer cancel()

			conn, err := forward.DialContext(subCtx)
			if err != nil {
				plog.G(ctx).Errorf("[Client] Failed to dial %s from %s: %v", forward.Addr, d.tun.LocalAddr(), err)
				time.Sleep(time.Second * 2)
				return
			}
			defer conn.Close()

			select {
			case d.reconnected <- struct{}{}:
			default:
			}

			udpConn := conn.(*UDPConnOverTCP)
			errChan := make(chan error, 2)
			go readFromConn(subCtx, udpConn, d.tunInbound, d.tunOutbound, errChan)
			go writeToConn(subCtx, udpConn.Conn, d.tunInbound, errChan)

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
	go handleGvisorPacket(gvisorInbound, tunInbound, 2).Run(ctx)
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

// writeToConn writes packets from tunInbound to the raw TCP conn with datagram framing.
// Packets in tunInbound have 2 bytes of headroom before the prefix byte (at buf[2]),
// allowing the datagram header to be written in-place at buf[0:2] without an extra copy.
func writeToConn(ctx context.Context, rawConn net.Conn, inbound <-chan *Packet, errChan chan error) {
	defer util.HandleCrash()
	for {
		select {
		case packet := <-inbound:
			if packet == nil {
				return
			}
			if err := rawConn.SetWriteDeadline(time.Now().Add(config.KeepAliveTime)); err != nil {
				plog.G(ctx).Errorf("[Client] Failed to set write deadline: %v", err)
				util.SafeWrite(errChan, errors.Wrap(err, "failed to set write deadline"))
				return
			}
			// Write datagram frame in-place: [2-byte length header][prefix+IP data]
			// Data layout: buf[0:2]=header, buf[2]=prefix, buf[3:]=IP packet
			binary.BigEndian.PutUint16(packet.data[:2], uint16(packet.length))
			_, err := rawConn.Write(packet.data[:packet.length+2])
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
	go handleGvisorPacket(gvisorInbound, d.tunOutbound, 0).Run(ctx)
	for ctx.Err() == nil {
		buf := config.LPool.Get().([]byte)[:]
		// Read at offset 3 to reserve [2-byte datagram header][1-byte prefix].
		// writeToConn writes the header in-place at buf[0:2], avoiding an extra pool alloc + memcpy.
		n, err := d.tun.Read(buf[3:])
		if err != nil {
			plog.G(ctx).Errorf("[Client] Failed to read from TUN: %v", err)
			util.SafeWrite(d.errChan, err)
			config.LPool.Put(buf[:])
			return
		}
		buf[2] = 1

		var src, dst net.IP
		var protocol int
		src, dst, protocol, err = util.ParseIP(buf[3 : 3+n])
		if err != nil {
			plog.G(ctx).Errorf("[Client] Unknown packet, dropping: %v", err)
			config.LPool.Put(buf[:])
			continue
		}
		plog.G(ctx).Debugf("[Client] SRC: %s, DST: %s, Protocol: %s, Length: %d", src, dst, layers.IPProtocol(protocol).String(), n)
		if src.Equal(dst) {
			// Self-to-self (rare): shift to offset 0 for gvisor input compatibility
			copy(buf[0:n+1], buf[2:3+n])
			gvisorInbound <- NewPacket(buf[:], n+1, src, dst)
		} else {
			// Hot path: prefix at buf[2], IP at buf[3:], headroom at buf[0:2] for datagram header
			d.tunInbound <- NewPacket(buf[:], n+1, src, dst)
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
		n := copy(buf[3:], payload)
		buf[2] = 1
		d.tunInbound <- &Packet{data: buf, length: n + 1}
	}

	sendAll := func() {
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

	sendAll()
	for ctx.Err() == nil {
		select {
		case <-ticker.C:
			sendAll()
		case <-d.reconnected:
			sendAll()
			ticker.Reset(config.KeepAliveTime)
		case <-ctx.Done():
			return
		}
	}
}
