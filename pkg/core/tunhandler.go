package core

import (
	"context"
	"net"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

const (
	MaxSize = 1000
)

type tunHandler struct {
	chain       *Chain
	node        *Node
	routeMapUDP *RouteMap
	// map[srcIP]net.Conn
	routeMapTCP *sync.Map
	chExit      chan error
}

type RouteMap struct {
	lock   *sync.RWMutex
	routes map[string]net.Addr
}

func NewRouteMap() *RouteMap {
	return &RouteMap{
		lock:   &sync.RWMutex{},
		routes: map[string]net.Addr{},
	}
}

func (n *RouteMap) LoadOrStore(to net.IP, addr net.Addr) (net.Addr, bool) {
	n.lock.RLock()
	route, load := n.routes[to.String()]
	n.lock.RUnlock()
	if load {
		return route, true
	}

	n.lock.Lock()
	defer n.lock.Unlock()
	n.routes[to.String()] = addr
	return addr, false
}

func (n *RouteMap) Store(to net.IP, addr net.Addr) {
	n.lock.Lock()
	defer n.lock.Unlock()
	n.routes[to.String()] = addr
}

func (n *RouteMap) RouteTo(ip net.IP) net.Addr {
	n.lock.RLock()
	defer n.lock.RUnlock()
	return n.routes[ip.String()]
}

// TunHandler creates a handler for tun tunnel.
func TunHandler(chain *Chain, node *Node) Handler {
	return &tunHandler{
		chain:       chain,
		node:        node,
		routeMapUDP: NewRouteMap(),
		routeMapTCP: RouteMapTCP,
		chExit:      make(chan error, 1),
	}
}

func (h *tunHandler) Handle(ctx context.Context, tun net.Conn) {
	if h.node.Remote != "" {
		h.HandleClient(ctx, tun)
	} else {
		h.HandleServer(ctx, tun)
	}
}

type Device struct {
	tun net.Conn

	tunInbound  chan *DataElem
	tunOutbound chan *DataElem

	// your main logic
	tunInboundHandler func(tunInbound <-chan *DataElem, tunOutbound chan<- *DataElem)

	chExit chan error
}

func (d *Device) readFromTun() {
	for {
		buf := config.SPool.Get().([]byte)[:]
		n, err := d.tun.Read(buf[:])
		if err != nil {
			config.SPool.Put(buf[:])
			log.Errorf("[TUN] Failed to read from tun: %v", err)
			util.SafeWrite(d.chExit, err)
			return
		}
		if n == 0 {
			log.Errorf("[TUN] Read packet length 0")
			config.SPool.Put(buf[:])
			continue
		}

		src, dst, err := util.ParseIP(buf[:n])
		if err != nil {
			log.Errorf("[TUN] Unknown packet")
			config.SPool.Put(buf[:])
			continue
		}

		log.Debugf("[TUN] SRC: %s --> DST: %s, length: %d", src, dst, n)
		util.SafeWrite(d.tunInbound, &DataElem{
			data:   buf[:],
			length: n,
			src:    src,
			dst:    dst,
		})
	}
}

func (d *Device) writeToTun() {
	for e := range d.tunOutbound {
		_, err := d.tun.Write(e.data[:e.length])
		config.SPool.Put(e.data[:])
		if err != nil {
			util.SafeWrite(d.chExit, err)
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

func heartbeats(ctx context.Context, tun net.Conn) {
	tunIfi, err := util.GetTunDeviceByConn(tun)
	if err != nil {
		log.Errorf("Failed to get tun device: %s", err.Error())
		return
	}
	srcIPv4, srcIPv6, dockerSrcIPv4, err := util.GetTunDeviceIP(tunIfi.Name)
	if err != nil {
		return
	}

	ticker := time.NewTicker(time.Second * 5)
	defer ticker.Stop()

	for ; true; <-ticker.C {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if srcIPv4 != nil {
			go util.Ping(ctx, srcIPv4.String(), config.RouterIP.String())
		}
		if srcIPv6 != nil {
			go util.Ping(ctx, srcIPv6.String(), config.RouterIP6.String())
		}
		if dockerSrcIPv4 != nil {
			go util.Ping(ctx, dockerSrcIPv4.String(), config.DockerRouterIP.String())
		}
	}
}

func (d *Device) Start(ctx context.Context) {
	go d.readFromTun()
	go d.tunInboundHandler(d.tunInbound, d.tunOutbound)
	go d.writeToTun()

	select {
	case err := <-d.chExit:
		log.Errorf("Device exit: %v", err)
		return
	case <-ctx.Done():
		return
	}
}

func (d *Device) SetTunInboundHandler(handler func(tunInbound <-chan *DataElem, tunOutbound chan<- *DataElem)) {
	d.tunInboundHandler = handler
}

func (h *tunHandler) HandleServer(ctx context.Context, tun net.Conn) {
	device := &Device{
		tun:         tun,
		tunInbound:  make(chan *DataElem, MaxSize),
		tunOutbound: make(chan *DataElem, MaxSize),
		chExit:      h.chExit,
	}
	device.SetTunInboundHandler(func(tunInbound <-chan *DataElem, tunOutbound chan<- *DataElem) {
		for ctx.Err() == nil {
			packetConn, err := (&net.ListenConfig{}).ListenPacket(ctx, "udp", h.node.Addr)
			if err != nil {
				log.Errorf("[UDP] Failed to listen %s: %v", h.node.Addr, err)
				return
			}
			err = transportTunServer(ctx, tunInbound, tunOutbound, packetConn, h.routeMapUDP, h.routeMapTCP)
			if err != nil {
				log.Errorf("[TUN] %s: %v", tun.LocalAddr(), err)
			}
		}
	})

	defer device.Close()
	device.Start(ctx)
}

type DataElem struct {
	data   []byte
	length int
	src    net.IP
	dst    net.IP
}

func NewDataElem(data []byte, length int, src net.IP, dst net.IP) *DataElem {
	return &DataElem{
		data:   data,
		length: length,
		src:    src,
		dst:    dst,
	}
}

func (d *DataElem) Data() []byte {
	return d.data
}

func (d *DataElem) Length() int {
	return d.length
}

type udpElem struct {
	from   net.Addr
	data   []byte
	length int
	src    net.IP
	dst    net.IP
}

type Peer struct {
	conn net.PacketConn

	connInbound chan *udpElem

	tunInbound  <-chan *DataElem
	tunOutbound chan<- *DataElem

	// map[srcIP.String()]net.Addr for udp
	routeMapUDP *RouteMap
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

func (p *Peer) readFromConn() {
	for {
		buf := config.SPool.Get().([]byte)[:]
		n, from, err := p.conn.ReadFrom(buf[:])
		if err != nil {
			config.SPool.Put(buf[:])
			p.sendErr(err)
			return
		}

		src, dst, err := util.ParseIP(buf[:n])
		if err != nil {
			config.SPool.Put(buf[:])
			log.Errorf("[TUN] Unknown packet: %v", err)
			continue
		}
		if addr, loaded := p.routeMapUDP.LoadOrStore(src, from); loaded {
			if addr.String() != from.String() {
				p.routeMapUDP.Store(src, from)
				log.Debugf("[TUN] Replace route map UDP: %s -> %s", src, from)
			}
		} else {
			log.Debugf("[TUN] Add new route map UDP: %s -> %s", src, from)
		}

		p.connInbound <- &udpElem{
			from:   from,
			data:   buf[:],
			length: n,
			src:    src,
			dst:    dst,
		}
	}
}

func (p *Peer) readFromTCPConn() {
	for packet := range TCPPacketChan {
		src, dst, err := util.ParseIP(packet.Data)
		if err != nil {
			log.Errorf("[TUN] Unknown packet")
			config.SPool.Put(packet.Data[:])
			continue
		}
		u := &udpElem{
			data:   packet.Data[:],
			length: int(packet.DataLength),
			src:    src,
			dst:    dst,
		}
		log.Debugf("[TCP] udp-tun %s >>> %s length: %d", u.src, u.dst, u.length)
		p.connInbound <- u
	}
}

func (p *Peer) routePeer() {
	for e := range p.connInbound {
		if routeToAddr := p.routeMapUDP.RouteTo(e.dst); routeToAddr != nil {
			log.Debugf("[UDP] Find UDP route to dst: %s -> %s", e.dst, routeToAddr)
			_, err := p.conn.WriteTo(e.data[:e.length], routeToAddr)
			config.SPool.Put(e.data[:])
			if err != nil {
				p.sendErr(err)
				return
			}
		} else if conn, ok := p.routeMapTCP.Load(e.dst.String()); ok {
			log.Debugf("[TCP] Find TCP route to dst: %s -> %s", e.dst.String(), conn.(net.Conn).RemoteAddr())
			dgram := newDatagramPacket(e.data[:e.length])
			err := dgram.Write(conn.(net.Conn))
			config.SPool.Put(e.data[:])
			if err != nil {
				log.Errorf("[TCP] udp-tun %s <- %s : %s", conn.(net.Conn).RemoteAddr(), dgram.Addr(), err)
				p.sendErr(err)
				return
			}
		} else {
			log.Debugf("[TUN] Not found route to dst: %s, write to TUN device", e.dst.String())
			p.tunOutbound <- &DataElem{
				data:   e.data,
				length: e.length,
				src:    e.src,
				dst:    e.dst,
			}
		}
	}
}

func (p *Peer) routeTUN() {
	for e := range p.tunInbound {
		if addr := p.routeMapUDP.RouteTo(e.dst); addr != nil {
			log.Debugf("[TUN] Find UDP route to dst: %s -> %s", e.dst, addr)
			_, err := p.conn.WriteTo(e.data[:e.length], addr)
			config.SPool.Put(e.data[:])
			if err != nil {
				log.Debugf("[TUN] Failed wirte to route dst: %s -> %s", e.dst, addr)
				p.sendErr(err)
				return
			}
		} else if conn, ok := p.routeMapTCP.Load(e.dst.String()); ok {
			log.Debugf("[TUN] Find TCP route to dst: %s -> %s", e.dst.String(), conn.(net.Conn).RemoteAddr())
			dgram := newDatagramPacket(e.data[:e.length])
			err := dgram.Write(conn.(net.Conn))
			config.SPool.Put(e.data[:])
			if err != nil {
				log.Errorf("[TUN] Failed to write TCP %s <- %s : %s", conn.(net.Conn).RemoteAddr(), dgram.Addr(), err)
				p.sendErr(err)
				return
			}
		} else {
			log.Errorf("[TUN] No route for src: %s -> dst: %s, drop it", e.src, e.dst)
			config.SPool.Put(e.data[:])
		}
	}
}

func (p *Peer) Start() {
	go p.readFromConn()
	go p.readFromTCPConn()
	go p.routePeer()
	go p.routeTUN()
}

func (p *Peer) Close() {
	p.conn.Close()
}

func transportTunServer(ctx context.Context, tunInbound <-chan *DataElem, tunOutbound chan<- *DataElem, packetConn net.PacketConn, routeMapUDP *RouteMap, routeMapTCP *sync.Map) error {
	p := &Peer{
		conn:        packetConn,
		connInbound: make(chan *udpElem, MaxSize),
		tunInbound:  tunInbound,
		tunOutbound: tunOutbound,
		routeMapUDP: routeMapUDP,
		routeMapTCP: routeMapTCP,
		errChan:     make(chan error, 2),
	}

	defer p.Close()
	p.Start()

	select {
	case err := <-p.errChan:
		log.Errorf(err.Error())
		return err
	case <-ctx.Done():
		return nil
	}
}
