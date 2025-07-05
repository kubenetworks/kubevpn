package core

import (
	"context"
	"net"
	"sync"

	"github.com/google/gopacket/layers"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

const (
	MaxSize = 1000
)

type tunHandler struct {
	forward     *Forwarder
	node        *Node
	routeMapTCP *sync.Map
	errChan     chan error
}

// TunHandler creates a handler for tun tunnel.
func TunHandler(forward *Forwarder, node *Node) Handler {
	return &tunHandler{
		forward:     forward,
		node:        node,
		routeMapTCP: RouteMapTCP,
		errChan:     make(chan error, 1),
	}
}

func (h *tunHandler) Handle(ctx context.Context, tun net.Conn) {
	if remote := h.node.Remote; remote != "" {
		h.HandleClient(ctx, tun)
	} else {
		h.HandleServer(ctx, tun)
	}
}

func (h *tunHandler) HandleServer(ctx context.Context, tun net.Conn) {
	device := &Device{
		tun:         tun,
		tunInbound:  make(chan *Packet, MaxSize),
		tunOutbound: make(chan *Packet, MaxSize),
		errChan:     h.errChan,
	}

	defer device.Close()
	go device.readFromTUN(ctx)
	go device.writeToTUN(ctx)
	go device.handlePacket(ctx, h.routeMapTCP)

	select {
	case err := <-device.errChan:
		plog.G(ctx).Errorf("Device exit: %v", err)
		return
	case <-ctx.Done():
		return
	}
}

type Device struct {
	tun net.Conn

	tunInbound  chan *Packet
	tunOutbound chan *Packet

	errChan chan error
}

func (d *Device) readFromTUN(ctx context.Context) {
	defer util.HandleCrash()
	for {
		buf := config.LPool.Get().([]byte)[:]
		n, err := d.tun.Read(buf[:])
		if err != nil {
			config.LPool.Put(buf[:])
			plog.G(ctx).Errorf("[TUN] Failed to read from tun device: %v", err)
			util.SafeWrite(d.errChan, err)
			return
		}

		src, dst, protocol, err := util.ParseIP(buf[:n])
		if err != nil {
			plog.G(ctx).Errorf("[TUN] Unknown packet")
			config.LPool.Put(buf[:])
			continue
		}

		plog.G(ctx).Debugf("[TUN] SRC: %s, DST: %s, Protocol: %s, Length: %d", src, dst, layers.IPProtocol(protocol).String(), n)
		util.SafeWrite(d.tunInbound, NewPacket(buf[:], n, src, dst), func(v *Packet) {
			config.LPool.Put(v.data[:])
			plog.G(context.Background()).Errorf("Drop packet, SRC: %s, DST: %s, Protocol: %s, Length: %d", v.src, v.dst, layers.IPProtocol(protocol).String(), v.length)
		})
	}
}

func (d *Device) writeToTUN(ctx context.Context) {
	defer util.HandleCrash()
	for packet := range d.tunOutbound {
		_, err := d.tun.Write(packet.data[1:packet.length])
		config.LPool.Put(packet.data[:])
		if err != nil {
			plog.G(ctx).Errorf("[TUN] Failed to write to tun device: %v", err)
			util.SafeWrite(d.errChan, err)
			return
		}
	}
}

func (d *Device) Close() {
	d.tun.Close()
	util.SafeClose(d.tunInbound)
	util.SafeClose(d.tunOutbound)
	util.SafeClose(TCPPacketChan)
}

func (d *Device) handlePacket(ctx context.Context, routeMapTCP *sync.Map) {
	p := &Peer{
		tunInbound:  d.tunInbound,
		tunOutbound: d.tunOutbound,
		routeMapTCP: routeMapTCP,
		errChan:     make(chan error, 1),
	}

	go p.routeTUN(ctx)
	go p.routeTCPToTun(ctx)

	select {
	case err := <-p.errChan:
		plog.G(ctx).Errorf("[TUN] %s: %v", d.tun.LocalAddr(), err)
		util.SafeWrite(d.errChan, err)
		return
	case <-ctx.Done():
		return
	}
}

type Packet struct {
	data   []byte
	length int
	src    net.IP
	dst    net.IP
}

func NewPacket(data []byte, length int, src net.IP, dst net.IP) *Packet {
	return &Packet{
		data:   data,
		length: length,
		src:    src,
		dst:    dst,
	}
}

func (d *Packet) Data() []byte {
	return d.data
}

func (d *Packet) Length() int {
	return d.length
}

type Peer struct {
	tunInbound  chan *Packet
	tunOutbound chan<- *Packet

	// map[srcIP.String()]net.Conn for tcp
	routeMapTCP *sync.Map

	errChan chan error
}

func (p *Peer) sendErr(err error) {
	select {
	case p.errChan <- err:
	default:
	}
}

func (p *Peer) routeTCPToTun(ctx context.Context) {
	defer util.HandleCrash()
	for packet := range TCPPacketChan {
		p.tunOutbound <- packet
	}
}

func (p *Peer) routeTUN(ctx context.Context) {
	defer util.HandleCrash()
	for packet := range p.tunInbound {
		if conn, ok := p.routeMapTCP.Load(packet.dst.String()); ok {
			plog.G(ctx).Debugf("[TUN] Find TCP route to dst: %s -> %s", packet.dst.String(), conn.(net.Conn).RemoteAddr())
			// local client handle it with gVisor
			copy(packet.data[1:packet.length+1], packet.data[:packet.length])
			dgram := newDatagramPacket(packet.data, packet.length+1)
			err := dgram.Write(conn.(net.Conn))
			config.LPool.Put(packet.data[:])
			if err != nil {
				plog.G(ctx).Errorf("[TUN] Failed to write TCP %s <- %s : %s", conn.(net.Conn).RemoteAddr(), conn.(net.Conn).LocalAddr(), err)
				p.sendErr(err)
				return
			}
		} else {
			plog.G(ctx).Warnf("[TUN] No route for src: %s -> dst: %s, drop it", packet.src, packet.dst)
			config.LPool.Put(packet.data[:])
		}
	}
}
