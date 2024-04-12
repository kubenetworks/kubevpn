package core

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"strings"
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
	chain    *Chain
	node     *Node
	routeNAT *NAT
	// map[srcIP]net.Conn
	routeConnNAT *sync.Map
	chExit       chan error
}

type NAT struct {
	lock   *sync.RWMutex
	routes map[string][]net.Addr
}

func NewNAT() *NAT {
	return &NAT{
		lock:   &sync.RWMutex{},
		routes: map[string][]net.Addr{},
	}
}

func (n *NAT) RemoveAddr(addr net.Addr) (count int) {
	n.lock.Lock()
	defer n.lock.Unlock()
	for k, v := range n.routes {
		for i := 0; i < len(v); i++ {
			if v[i].String() == addr.String() {
				v = append(v[:i], v[i+1:]...)
				i--
				count++
			}
		}
		n.routes[k] = v
	}
	return
}

func (n *NAT) LoadOrStore(to net.IP, addr net.Addr) (result net.Addr, load bool) {
	n.lock.RLock()
	addrList := n.routes[to.String()]
	n.lock.RUnlock()
	for _, add := range addrList {
		if add.String() == addr.String() {
			load = true
			result = addr
			return
		}
	}

	n.lock.Lock()
	defer n.lock.Unlock()
	if addrList == nil {
		n.routes[to.String()] = []net.Addr{addr}
		result = addr
		return
	} else {
		n.routes[to.String()] = append(n.routes[to.String()], addr)
		result = addr
		return
	}
}

func (n *NAT) RouteTo(ip net.IP) net.Addr {
	n.lock.RLock()
	defer n.lock.RUnlock()
	addrList := n.routes[ip.String()]
	if len(addrList) == 0 {
		return nil
	}
	// for load balance
	index := rand.Intn(len(n.routes[ip.String()]))
	return addrList[index]
}

func (n *NAT) Remove(ip net.IP, addr net.Addr) {
	n.lock.Lock()
	defer n.lock.Unlock()

	addrList, ok := n.routes[ip.String()]
	if !ok {
		return
	}
	for i := 0; i < len(addrList); i++ {
		if addrList[i].String() == addr.String() {
			addrList = append(addrList[:i], addrList[i+1:]...)
			i--
		}
	}
	n.routes[ip.String()] = addrList
	return
}

func (n *NAT) Range(f func(key string, v []net.Addr)) {
	n.lock.RLock()
	defer n.lock.RUnlock()
	for k, v := range n.routes {
		f(k, v)
	}
}

// TunHandler creates a handler for tun tunnel.
func TunHandler(chain *Chain, node *Node) Handler {
	return &tunHandler{
		chain:        chain,
		node:         node,
		routeNAT:     RouteNAT,
		routeConnNAT: RouteConnNAT,
		chExit:       make(chan error, 1),
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
	var sb strings.Builder
	var i int
	for ctx.Err() == nil {
		select {
		case <-ticker.C:
			i = 0
			sb.Reset()
			h.routeNAT.Range(func(key string, value []net.Addr) {
				i++
				var s []string
				for _, addr := range value {
					if addr != nil {
						s = append(s, addr.String())
					}
				}
				if len(s) != 0 {
					sb.WriteString(fmt.Sprintf("to: %s, route: %s\n", key, strings.Join(s, " ")))
				}
			})
			log.Debug(sb.String())
			log.Debug(i)
		}
	}
}

type Device struct {
	tun net.Conn

	tunInboundRaw chan *DataElem
	tunInbound    chan *DataElem
	tunOutbound   chan *DataElem

	// your main logic
	tunInboundHandler func(tunInbound <-chan *DataElem, tunOutbound chan<- *DataElem)

	chExit chan error
}

func (d *Device) readFromTun() {
	for {
		b := config.LPool.Get().([]byte)[:]
		n, err := d.tun.Read(b[:])
		if err != nil {
			select {
			case d.chExit <- err:
			default:
			}
			return
		}
		if n != 0 {
			d.tunInboundRaw <- &DataElem{
				data:   b[:],
				length: n,
			}
		}
	}
}

func (d *Device) writeToTun() {
	for e := range d.tunOutbound {
		_, err := d.tun.Write(e.data[:e.length])
		config.LPool.Put(e.data[:])
		if err != nil {
			select {
			case d.chExit <- err:
			default:
			}
			return
		}
	}
}

func (d *Device) parseIPHeader() {
	for e := range d.tunInboundRaw {
		if util.IsIPv4(e.data[:e.length]) {
			// ipv4.ParseHeader
			b := e.data[:e.length]
			e.src = net.IPv4(b[12], b[13], b[14], b[15])
			e.dst = net.IPv4(b[16], b[17], b[18], b[19])
		} else if util.IsIPv6(e.data[:e.length]) {
			// ipv6.ParseHeader
			e.src = e.data[:e.length][8:24]
			e.dst = e.data[:e.length][24:40]
		} else {
			log.Errorf("[tun-packet] unknown packet")
			continue
		}

		log.Debugf("[tun] %s --> %s, length: %d", e.src, e.dst, e.length)
		d.tunInbound <- e
	}
}

func (d *Device) Close() {
	d.tun.Close()
}

func heartbeats(tun net.Conn, in chan<- *DataElem) {
	conn, err := util.GetTunDeviceByConn(tun)
	if err != nil {
		log.Errorf("get tun device error: %s", err.Error())
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

	var bytes []byte
	var bytes6 []byte

	ticker := time.NewTicker(time.Second * 5)
	defer ticker.Stop()

	for ; true; <-ticker.C {
		for i := 0; i < 4; i++ {
			if bytes == nil {
				bytes, err = genICMPPacket(srcIPv4, config.RouterIP)
				if err != nil {
					log.Errorf("generate ipv4 packet error: %s", err.Error())
					continue
				}
			}
			if bytes6 == nil {
				bytes6, err = genICMPPacketIPv6(srcIPv6, config.RouterIP6)
				if err != nil {
					log.Errorf("generate ipv6 packet error: %s", err.Error())
					continue
				}
			}
			for index, i2 := range [][]byte{bytes, bytes6} {
				data := config.LPool.Get().([]byte)[:]
				length := copy(data, i2)
				var src, dst net.IP
				if index == 0 {
					src, dst = srcIPv4, config.RouterIP
				} else {
					src, dst = srcIPv6, config.RouterIP6
				}
				in <- &DataElem{
					data:   data[:],
					length: length,
					src:    src,
					dst:    dst,
				}
			}
			time.Sleep(time.Second)
		}
	}
}

func genICMPPacket(src net.IP, dst net.IP) ([]byte, error) {
	buf := gopacket.NewSerializeBuffer()
	icmpLayer := layers.ICMPv4{
		TypeCode: layers.CreateICMPv4TypeCode(layers.ICMPv4TypeEchoRequest, 0),
		Id:       3842,
		Seq:      1,
	}
	ipLayer := layers.IPv4{
		Version:  4,
		SrcIP:    src,
		DstIP:    dst,
		Protocol: layers.IPProtocolICMPv4,
		Flags:    layers.IPv4DontFragment,
		TTL:      64,
		IHL:      5,
		Id:       55664,
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
	go d.parseIPHeader()
	go d.tunInboundHandler(d.tunInbound, d.tunOutbound)
	go d.writeToTun()
	go heartbeats(d.tun, d.tunInbound)

	select {
	case err := <-d.chExit:
		log.Errorf("device exit: %s", err.Error())
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
		tun:           tun,
		tunInboundRaw: make(chan *DataElem, MaxSize),
		tunInbound:    make(chan *DataElem, MaxSize),
		tunOutbound:   make(chan *DataElem, MaxSize),
		chExit:        h.chExit,
	}
	device.SetTunInboundHandler(func(tunInbound <-chan *DataElem, tunOutbound chan<- *DataElem) {
		for {
			packetConn, err := (&net.ListenConfig{}).ListenPacket(ctx, "udp", h.node.Addr)
			if err != nil {
				log.Debugf("[udp] can not listen %s, err: %v", h.node.Addr, err)
				return
			}
			err = transportTun(ctx, tunInbound, tunOutbound, packetConn, h.routeNAT, h.routeConnNAT)
			if err != nil {
				log.Debugf("[tun] %s: %v", tun.LocalAddr(), err)
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

	connInbound    chan *udpElem
	parsedConnInfo chan *udpElem

	tunInbound  <-chan *DataElem
	tunOutbound chan<- *DataElem

	routeNAT *NAT
	// map[srcIP]net.Conn
	// 	routeConnNAT sync.Map
	routeConnNAT *sync.Map

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
		n, srcAddr, err := p.conn.ReadFrom(b[:])
		if err != nil {
			p.sendErr(err)
			return
		}
		p.connInbound <- &udpElem{
			from:   srcAddr,
			data:   b[:],
			length: n,
		}
	}
}

func (p *Peer) readFromTCPConn() {
	for packet := range Chan {
		u := &udpElem{
			data:   packet.Data[:],
			length: int(packet.DataLength),
		}
		b := packet.Data
		if util.IsIPv4(packet.Data) {
			// ipv4.ParseHeader
			u.src = net.IPv4(b[12], b[13], b[14], b[15])
			u.dst = net.IPv4(b[16], b[17], b[18], b[19])
		} else if util.IsIPv6(packet.Data) {
			// ipv6.ParseHeader
			u.src = b[8:24]
			u.dst = b[24:40]
		} else {
			log.Errorf("[tun-conn] unknown packet")
			continue
		}
		log.Debugf("[tcpserver] udp-tun %s >>> %s length: %d", u.src, u.dst, u.length)
		p.parsedConnInfo <- u
	}
}

func (p *Peer) parseHeader() {
	var firstIPv4, firstIPv6 = true, true
	for e := range p.connInbound {
		b := e.data[:e.length]
		if util.IsIPv4(e.data[:e.length]) {
			// ipv4.ParseHeader
			e.src = net.IPv4(b[12], b[13], b[14], b[15])
			e.dst = net.IPv4(b[16], b[17], b[18], b[19])
		} else if util.IsIPv6(e.data[:e.length]) {
			// ipv6.ParseHeader
			e.src = b[:e.length][8:24]
			e.dst = b[:e.length][24:40]
		} else {
			log.Errorf("[tun] unknown packet")
			continue
		}

		if firstIPv4 || firstIPv6 {
			if util.IsIPv4(e.data[:e.length]) {
				firstIPv4 = false
			} else {
				firstIPv6 = false
			}
			if _, loaded := p.routeNAT.LoadOrStore(e.src, e.from); loaded {
				log.Debugf("[tun] find route: %s -> %s", e.src, e.from)
			} else {
				log.Debugf("[tun] new route: %s -> %s", e.src, e.from)
			}
		}
		p.parsedConnInfo <- e
	}
}

func (p *Peer) routePeer() {
	for e := range p.parsedConnInfo {
		if routeToAddr := p.routeNAT.RouteTo(e.dst); routeToAddr != nil {
			log.Debugf("[tun] find route: %s -> %s", e.dst, routeToAddr)
			_, err := p.conn.WriteTo(e.data[:e.length], routeToAddr)
			config.LPool.Put(e.data[:])
			if err != nil {
				p.sendErr(err)
				return
			}
		} else if conn, ok := p.routeConnNAT.Load(e.dst.String()); ok {
			dgram := newDatagramPacket(e.data[:e.length])
			if err := dgram.Write(conn.(net.Conn)); err != nil {
				log.Debugf("[tcpserver] udp-tun %s <- %s : %s", conn.(net.Conn).RemoteAddr(), dgram.Addr(), err)
				p.sendErr(err)
				return
			}
			config.LPool.Put(e.data[:])
		} else {
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
		if addr := p.routeNAT.RouteTo(e.dst); addr != nil {
			log.Debugf("[tun] find route: %s -> %s", e.dst, addr)
			_, err := p.conn.WriteTo(e.data[:e.length], addr)
			config.LPool.Put(e.data[:])
			if err != nil {
				log.Debugf("[tun] can not route: %s -> %s", e.dst, addr)
				p.sendErr(err)
				return
			}
		} else if conn, ok := p.routeConnNAT.Load(e.dst.String()); ok {
			dgram := newDatagramPacket(e.data[:e.length])
			err := dgram.Write(conn.(net.Conn))
			config.LPool.Put(e.data[:])
			if err != nil {
				log.Debugf("[tcpserver] udp-tun %s <- %s : %s", conn.(net.Conn).RemoteAddr(), dgram.Addr(), err)
				p.sendErr(err)
				return
			}
		} else {
			config.LPool.Put(e.data[:])
			log.Debug(fmt.Errorf("[tun] no route for %s -> %s", e.src, e.dst))
		}
	}
}

func (p *Peer) Start() {
	go p.readFromConn()
	go p.readFromTCPConn()
	go p.parseHeader()
	go p.routePeer()
	go p.routeTUN()
}

func (p *Peer) Close() {
	p.conn.Close()
}

func transportTun(ctx context.Context, tunInbound <-chan *DataElem, tunOutbound chan<- *DataElem, packetConn net.PacketConn, nat *NAT, connNAT *sync.Map) error {
	p := &Peer{
		conn:           packetConn,
		connInbound:    make(chan *udpElem, MaxSize),
		parsedConnInfo: make(chan *udpElem, MaxSize),
		tunInbound:     tunInbound,
		tunOutbound:    tunOutbound,
		routeNAT:       nat,
		routeConnNAT:   connNAT,
		errChan:        make(chan error, 2),
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
