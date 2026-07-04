package core

import (
	"context"
	"encoding/binary"
	"net"

	"github.com/google/gopacket/layers"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type tunHandler struct {
	forward *Forwarder
	hub     *RouteHub
	errChan chan error
}

// TunHandler creates a handler for tun tunnel.
func TunHandler(forward *Forwarder, hub *RouteHub) Handler {
	if hub == nil {
		hub = DefaultRouteHub
	}
	return &tunHandler{
		forward: forward,
		hub:     hub,
		errChan: make(chan error, 1),
	}
}

func (h *tunHandler) Handle(ctx context.Context, tun net.Conn) {
	tunIfi, err := util.GetTunDeviceByConn(tun)
	if err != nil {
		plog.G(ctx).Errorf("[TUN] Failed to get tun device: %v", err)
		return
	}
	ctx = plog.WithField(ctx, tunIfi.Name, "")
	if !h.forward.IsEmpty() {
		h.HandleClient(ctx, tun, h.forward)
	} else {
		h.HandleServer(ctx, tun)
	}
}

func (h *tunHandler) HandleServer(ctx context.Context, tun net.Conn) {
	device := &Device{tunDevice{
		tun:         tun,
		tunInbound:  make(chan *Packet, MaxSize),
		tunOutbound: make(chan *Packet, MaxSize),
		errChan:     h.errChan,
	}}

	defer device.Close()
	go device.readFromTun(ctx)
	go device.writeToTun(ctx)
	go device.handlePacket(ctx, h.hub)

	select {
	case err := <-device.errChan:
		plog.G(ctx).Errorf("[TUN] Device exit: %v", err)
		return
	case <-ctx.Done():
		return
	}
}

// Device is the server-side tun device handler.
type Device struct {
	tunDevice
}

func (d *Device) readFromTun(ctx context.Context) {
	defer util.HandleCrash()
	for ctx.Err() == nil {
		buf := config.LPool.Get().([]byte)[:]
		// Read at offset 3 to reserve space for [2-byte datagram header][1-byte type prefix].
		// This avoids two memcpys later in routeTun (shift+1) and DatagramPacket.Write (shift+2).
		n, err := d.tun.Read(buf[3:])
		if err != nil {
			config.LPool.Put(buf[:])
			plog.G(ctx).Errorf("[TUN] Failed to read from tun device: %v", err)
			util.SafeWrite(d.errChan, err)
			return
		}

		src, dst, protocol, parseErr := util.ParseIPFast(buf[3 : 3+n])
		if parseErr != nil {
			plog.G(ctx).Errorf("[TUN] Unknown packet, dropping")
			config.LPool.Put(buf[:])
			continue
		}
		if config.Debug {
			plog.G(ctx).Debugf("[TUN] SRC: %s, DST: %s, Protocol: %s, Length: %d", src, dst, layers.IPProtocol(protocol).String(), n)
		}
		d.tunInbound <- NewPacket(buf[:], n, src, dst)
	}
}

func (d *Device) handlePacket(ctx context.Context, hub *RouteHub) {
	p := &Peer{
		tunInbound:  d.tunInbound,
		tunOutbound: d.tunOutbound,
		hub:         hub,
		errChan:     make(chan error, 1),
	}

	go p.routeTun(ctx)
	go p.routeTCPToTun(ctx)

	select {
	case err := <-p.errChan:
		plog.G(ctx).Errorf("[TUN] Peer error on %s: %v", d.tun.LocalAddr(), err)
		util.SafeWrite(d.errChan, err)
		return
	case <-ctx.Done():
		return
	}
}

// Peer manages the routing between TUN device packets and the connection pool.
type Peer struct {
	tunInbound  chan *Packet
	tunOutbound chan<- *Packet

	hub *RouteHub

	errChan chan error
}

func (p *Peer) routeTCPToTun(ctx context.Context) {
	defer util.HandleCrash()
	for {
		select {
		case packet := <-p.hub.TCPPacketChan:
			if packet == nil {
				return
			}
			p.tunOutbound <- packet
		case <-ctx.Done():
			return
		}
	}
}

func (p *Peer) routeTun(ctx context.Context) {
	defer util.HandleCrash()
	for {
		select {
		case packet := <-p.tunInbound:
			if packet == nil {
				return
			}
			dstKey := string(packet.dst)
			// readFromTun placed IP data at buf[3:], leaving room for:
			//   buf[0:2] = datagram length header
			//   buf[2]   = type prefix byte
			//   buf[3:]  = IP packet data
			payloadLen := packet.length + 1
			binary.BigEndian.PutUint16(packet.data[:2], uint16(payloadLen))
			packet.data[2] = 1
			conn, err := p.hub.WriteToRoute(dstKey, packet.data[:payloadLen+2])
			config.LPool.Put(packet.data[:])
			if err != nil {
				if config.Debug {
					plog.G(ctx).Warnf("[TUN] No route for %s -> %s, dropping", packet.src, packet.dst)
				}
			} else if config.Debug {
				plog.G(ctx).Debugf("[TUN] Routed %s -> %s via %s", packet.src, packet.dst, conn.RemoteAddr())
			}
		case <-ctx.Done():
			return
		}
	}
}
