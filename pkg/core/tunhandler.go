package core

import (
	"context"
	"net"
	"sync"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

const (
	MaxSize = 100
)

type tunHandler struct {
	forward     *Forwarder
	node        *Node
	routeMapUDP *sync.Map
	routeMapTCP *sync.Map
	errChan     chan error
}

// TunHandler creates a handler for tun tunnel.
func TunHandler(forward *Forwarder, node *Node) Handler {
	return &tunHandler{
		forward:     forward,
		node:        node,
		routeMapUDP: &sync.Map{},
		routeMapTCP: RouteMapTCP,
		errChan:     make(chan error, 1),
	}
}

func (h *tunHandler) Handle(ctx context.Context, tun net.Conn) {
	if remote := h.node.Remote; remote != "" {
		remoteAddr, err := net.ResolveUDPAddr("udp", remote)
		if err != nil {
			plog.G(ctx).Errorf("[TUN-CLIENT] Failed to resolve udp addr %s: %v", remote, err)
			return
		}
		h.HandleClient(ctx, tun, remoteAddr)
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
	go device.transport(ctx, h.node.Addr, h.routeMapUDP, h.routeMapTCP)

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

		src, dst, err := util.ParseIP(buf[:n])
		if err != nil {
			plog.G(ctx).Errorf("[TUN] Unknown packet")
			config.LPool.Put(buf[:])
			continue
		}

		plog.G(ctx).Debugf("[TUN] SRC: %s, DST: %s, Length: %d", src, dst, n)
		util.SafeWrite(d.tunInbound, &Packet{
			data:   buf[:],
			length: n,
			src:    src,
			dst:    dst,
		}, func(v *Packet) {
			config.LPool.Put(v.data[:])
			plog.G(context.Background()).Errorf("Drop packet, SRC: %s, DST: %s, Length: %d", v.src, v.dst, v.length)
		})
	}
}

func (d *Device) writeToTUN(ctx context.Context) {
	defer util.HandleCrash()
	for packet := range d.tunOutbound {
		_, err := d.tun.Write(packet.data[:packet.length])
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

func (d *Device) transport(ctx1 context.Context, addr string, routeMapUDP *sync.Map, routeMapTCP *sync.Map) {
	for ctx1.Err() == nil {
		func() {
			ctx, cancelFunc := context.WithCancel(ctx1)
			defer cancelFunc()

			packetConn, err := (&net.ListenConfig{}).ListenPacket(ctx, "udp", addr)
			if err != nil {
				plog.G(ctx1).Errorf("[UDP] Failed to listen %s: %v", addr, err)
				return
			}

			p := &Peer{
				conn:        packetConn,
				tcpInbound:  make(chan *Packet, MaxSize),
				tunInbound:  d.tunInbound,
				tunOutbound: d.tunOutbound,
				routeMapUDP: routeMapUDP,
				routeMapTCP: routeMapTCP,
				errChan:     d.errChan,
			}

			defer p.Close()
			go p.readFromConn(ctx)
			go p.readFromTCPConn(ctx)
			go p.routeTCP(ctx)
			go p.routeTUN(ctx)

			select {
			case err = <-p.errChan:
				plog.G(ctx1).Errorf("[TUN] %s: %v", d.tun.LocalAddr(), err)
				return
			case <-ctx.Done():
				return
			}
		}()
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
	conn net.PacketConn

	tcpInbound chan *Packet

	tunInbound  chan *Packet
	tunOutbound chan<- *Packet

	// map[srcIP.String()]net.Addr for udp
	routeMapUDP *sync.Map
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

func (p *Peer) readFromConn(ctx context.Context) {
	defer util.HandleCrash()
	for ctx.Err() == nil {
		buf := config.LPool.Get().([]byte)[:]
		n, from, err := p.conn.ReadFrom(buf[:])
		if err != nil {
			config.LPool.Put(buf[:])
			p.sendErr(err)
			return
		}

		src, dst, err := util.ParseIP(buf[:n])
		if err != nil {
			config.LPool.Put(buf[:])
			plog.G(ctx).Errorf("[TUN] Unknown packet: %v", err)
			continue
		}
		if addr, loaded := p.routeMapUDP.LoadOrStore(src.String(), from); loaded {
			if addr.(net.Addr).String() != from.String() {
				p.routeMapUDP.Store(src.String(), from)
				plog.G(ctx).Infof("[TUN] Replace route map UDP: %s -> %s", src, from)
			}
		} else {
			plog.G(ctx).Infof("[TUN] Add new route map UDP: %s -> %s", src, from)
		}

		p.tunInbound <- &Packet{
			data:   buf[:],
			length: n,
			src:    src,
			dst:    dst,
		}
	}
}

func (p *Peer) readFromTCPConn(ctx context.Context) {
	defer util.HandleCrash()
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case packet := <-TCPPacketChan:
			src, dst, err := util.ParseIP(packet.Data)
			if err != nil {
				plog.G(ctx).Errorf("[TCP] Unknown packet")
				config.LPool.Put(packet.Data[:])
				continue
			}
			plog.G(ctx).Debugf("[TCP] SRC: %s > DST: %s Length: %d", src, dst, packet.DataLength)
			p.tcpInbound <- &Packet{
				data:   packet.Data[:],
				length: int(packet.DataLength),
				src:    src,
				dst:    dst,
			}
		}
	}
}

func (p *Peer) routeTCP(ctx context.Context) {
	defer util.HandleCrash()
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case packet := <-p.tcpInbound:
			if conn, ok := p.routeMapTCP.Load(packet.dst.String()); ok {
				plog.G(ctx).Debugf("[TCP] Find TCP route SRC: %s to DST: %s -> %s", packet.src.String(), packet.dst.String(), conn.(net.Conn).RemoteAddr())
				dgram := newDatagramPacket(packet.data[:packet.length])
				err := dgram.Write(conn.(net.Conn))
				config.LPool.Put(packet.data[:])
				if err != nil {
					plog.G(ctx).Errorf("[TCP] Failed to write to %s <- %s : %s", conn.(net.Conn).RemoteAddr(), conn.(net.Conn).LocalAddr(), err)
					p.sendErr(err)
					return
				}
			} else {
				plog.G(ctx).Debugf("[TCP] Not found route, write to TUN device. SRC: %s, DST: %s", packet.src.String(), packet.dst.String())
				p.tunOutbound <- &Packet{
					data:   packet.data,
					length: packet.length,
					src:    packet.src,
					dst:    packet.dst,
				}
			}
		}
	}
}

func (p *Peer) routeTUN(ctx context.Context) {
	defer util.HandleCrash()
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case packet := <-p.tunInbound:
			if addr, ok := p.routeMapUDP.Load(packet.dst.String()); ok {
				plog.G(ctx).Debugf("[TUN] Find UDP route to DST: %s -> %s, SRC: %s, DST: %s", packet.dst, addr, packet.src.String(), packet.dst.String())
				_, err := p.conn.WriteTo(packet.data[:packet.length], addr.(net.Addr))
				config.LPool.Put(packet.data[:])
				if err != nil {
					plog.G(ctx).Errorf("[TUN] Failed wirte to route dst: %s -> %s", packet.dst, addr)
					p.sendErr(err)
					return
				}
			} else if conn, ok := p.routeMapTCP.Load(packet.dst.String()); ok {
				plog.G(ctx).Debugf("[TUN] Find TCP route to dst: %s -> %s", packet.dst.String(), conn.(net.Conn).RemoteAddr())
				dgram := newDatagramPacket(packet.data[:packet.length])
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
}

func (p *Peer) Close() {
	p.conn.Close()
	util.SafeClose(p.tcpInbound)
}
