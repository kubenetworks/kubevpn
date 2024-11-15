package core

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
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

func (n *RouteMap) LoadOrStore(to net.IP, addr net.Addr) (result net.Addr, load bool) {
	n.lock.RLock()
	route, ok := n.routes[to.String()]
	n.lock.RUnlock()
	if ok && route.String() == addr.String() {
		return addr, true
	}

	n.lock.Lock()
	defer n.lock.Unlock()
	n.routes[to.String()] = addr
	return addr, false
}

func (n *RouteMap) RouteTo(ip net.IP) net.Addr {
	n.lock.RLock()
	defer n.lock.RUnlock()
	return n.routes[ip.String()]
}

func (n *RouteMap) Range(f func(key string, value net.Addr)) {
	n.lock.RLock()
	defer n.lock.RUnlock()
	for k, v := range n.routes {
		f(k, v)
	}
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

func (h *tunHandler) printRoute(ctx context.Context) {
	ticker := time.NewTicker(time.Second * 5)
	defer ticker.Stop()
	for ctx.Err() == nil {
		select {
		case <-ticker.C:
			h.routeMapUDP.Range(func(key string, value net.Addr) {
				log.Debugf("To: %s, route: %s", key, value.String())
			})
		}
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
		b := config.LPool.Get().([]byte)[:]
		n, err := d.tun.Read(b[:])
		if err != nil {
			log.Errorf("[TUN] Failed to read from tun: %v", err)
			util.SafeWrite(d.chExit, err)
			return
		}
		if n == 0 {
			log.Errorf("[TUN] Read packet length 0")
			continue
		}

		src, dst, err := util.ParseIP(b[:n])
		if err != nil {
			log.Errorf("[TUN] Unknown packet")
			continue
		}

		log.Debugf("[TUN] SRC: %s --> DST: %s, length: %d", src, dst, n)
		util.SafeWrite(d.tunInbound, &DataElem{
			data:   b[:],
			length: n,
			src:    src,
			dst:    dst,
		})
	}
}

func (d *Device) writeToTun() {
	for e := range d.tunOutbound {
		_, err := d.tun.Write(e.data[:e.length])
		config.LPool.Put(e.data[:])
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
	conn, err := util.GetTunDeviceByConn(tun)
	if err != nil {
		log.Errorf("Failed to get tun device: %s", err.Error())
		return
	}
	srcIPv4, srcIPv6, err := util.GetLocalTunIP(conn.Name)
	if err != nil {
		return
	}
	if config.RouterIP.To4().Equal(srcIPv4) {
		return
	}
	if config.RouterIP6.To4().Equal(srcIPv6) {
		return
	}
	var dstIPv4, dstIPv6 = net.IPv4zero, net.IPv6zero
	if config.CIDR.Contains(srcIPv4) {
		dstIPv4, dstIPv6 = config.RouterIP, config.RouterIP6
	} else if config.DockerCIDR.Contains(srcIPv4) {
		dstIPv4 = config.DockerRouterIP
	}

	ticker := time.NewTicker(time.Second * 5)
	defer ticker.Stop()

	for ; true; <-ticker.C {
		select {
		case <-ctx.Done():
			return
		default:
		}

		var src, dst net.IP
		src, dst = srcIPv4, dstIPv4
		if !dst.IsUnspecified() {
			_, _ = util.Ping(ctx, src.String(), dst.String())
		}
		src, dst = srcIPv6, dstIPv6
		if !dst.IsUnspecified() {
			_, _ = util.Ping(ctx, src.String(), dst.String())
		}
	}
}

func genICMPPacket(src net.IP, dst net.IP) ([]byte, error) {
	buf := gopacket.NewSerializeBuffer()
	var id uint16
	for _, b := range src {
		id += uint16(b)
	}
	icmpLayer := layers.ICMPv4{
		TypeCode: layers.CreateICMPv4TypeCode(layers.ICMPv4TypeEchoRequest, 0),
		Id:       id,
		Seq:      uint16(rand.Intn(math.MaxUint16 + 1)),
	}
	ipLayer := layers.IPv4{
		Version:  4,
		SrcIP:    src,
		DstIP:    dst,
		Protocol: layers.IPProtocolICMPv4,
		Flags:    layers.IPv4DontFragment,
		TTL:      64,
		IHL:      5,
		Id:       uint16(rand.Intn(math.MaxUint16 + 1)),
	}
	opts := gopacket.SerializeOptions{
		FixLengths:       true,
		ComputeChecksums: true,
	}
	err := gopacket.SerializeLayers(buf, opts, &ipLayer, &icmpLayer)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize icmp packet, err: %v", err)
	}
	return buf.Bytes(), nil
}

func genICMPPacketIPv6(src net.IP, dst net.IP) ([]byte, error) {
	buf := gopacket.NewSerializeBuffer()
	icmpLayer := layers.ICMPv6{
		TypeCode: layers.CreateICMPv6TypeCode(layers.ICMPv6TypeEchoRequest, 0),
	}
	ipLayer := layers.IPv6{
		Version:    6,
		SrcIP:      src,
		DstIP:      dst,
		NextHeader: layers.IPProtocolICMPv6,
		HopLimit:   255,
	}
	opts := gopacket.SerializeOptions{
		FixLengths: true,
	}
	err := gopacket.SerializeLayers(buf, opts, &ipLayer, &icmpLayer)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize icmp6 packet, err: %v", err)
	}
	return buf.Bytes(), nil
}

func (d *Device) Start(ctx context.Context) {
	go d.readFromTun()
	go d.tunInboundHandler(d.tunInbound, d.tunOutbound)
	go d.writeToTun()
	go heartbeats(ctx, d.tun)

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
	go h.printRoute(ctx)

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
			err = transportTun(ctx, tunInbound, tunOutbound, packetConn, h.routeMapUDP, h.routeMapTCP)
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
		b := config.LPool.Get().([]byte)[:]
		n, from, err := p.conn.ReadFrom(b[:])
		if err != nil {
			p.sendErr(err)
			return
		}

		src, dst, err := util.ParseIP(b[:n])
		if err != nil {
			log.Errorf("[TUN] Unknown packet: %v", err)
			continue
		}
		if _, loaded := p.routeMapUDP.LoadOrStore(src, from); loaded {
			log.Debugf("[TUN] Find route: %s -> %s", src, from)
		} else {
			log.Debugf("[TUN] Add new route: %s -> %s", src, from)
		}

		p.connInbound <- &udpElem{
			from:   from,
			data:   b[:],
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
			log.Debugf("[TCP] Find route: %s -> %s", e.dst, routeToAddr)
			_, err := p.conn.WriteTo(e.data[:e.length], routeToAddr)
			config.LPool.Put(e.data[:])
			if err != nil {
				p.sendErr(err)
				return
			}
		} else if conn, ok := p.routeMapTCP.Load(e.dst.String()); ok {
			log.Debugf("[TCP] Find TCP route to dst: %s -> %s", e.dst.String(), conn.(net.Conn).RemoteAddr())
			dgram := newDatagramPacket(e.data[:e.length])
			if err := dgram.Write(conn.(net.Conn)); err != nil {
				log.Errorf("[TCP] udp-tun %s <- %s : %s", conn.(net.Conn).RemoteAddr(), dgram.Addr(), err)
				p.sendErr(err)
				return
			}
			config.LPool.Put(e.data[:])
		} else {
			log.Debugf("[TCP] Not found route to dst: %s, write to TUN device", e.dst.String())
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
			log.Debugf("[TUN] Find route: %s -> %s", e.dst, addr)
			_, err := p.conn.WriteTo(e.data[:e.length], addr)
			config.LPool.Put(e.data[:])
			if err != nil {
				log.Debugf("[TUN] Failed to route: %s -> %s", e.dst, addr)
				p.sendErr(err)
				return
			}
		} else if conn, ok := p.routeMapTCP.Load(e.dst.String()); ok {
			log.Debugf("[TUN] Find TCP route to dst: %s -> %s", e.dst.String(), conn.(net.Conn).RemoteAddr())
			dgram := newDatagramPacket(e.data[:e.length])
			err := dgram.Write(conn.(net.Conn))
			config.LPool.Put(e.data[:])
			if err != nil {
				log.Errorf("[TUN] udp-tun %s <- %s : %s", conn.(net.Conn).RemoteAddr(), dgram.Addr(), err)
				p.sendErr(err)
				return
			}
		} else {
			log.Errorf("[TUN] No route for %s -> %s, drop it", e.src, e.dst)
			config.LPool.Put(e.data[:])
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

func transportTun(ctx context.Context, tunInbound <-chan *DataElem, tunOutbound chan<- *DataElem, packetConn net.PacketConn, routeMapUDP *RouteMap, routeMapTCP *sync.Map) error {
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
