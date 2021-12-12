package core

import (
	"context"
	"errors"
	"fmt"
	"github.com/wencaiwulue/kubevpn/util"
	"net"
	"sync"
	"time"

	"github.com/shadowsocks/go-shadowsocks2/shadowaead"
	log "github.com/sirupsen/logrus"
	"github.com/songgao/water/waterutil"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

func ipToTunRouteKey(ip net.IP) string {
	return ip.To16().String()
}

type tunHandler struct {
	options *HandlerOptions
	routes  sync.Map
	chExit  chan struct{}
}

// TunHandler creates a handler for tun tunnel.
func TunHandler(opts ...HandlerOption) Handler {
	h := &tunHandler{
		options: &HandlerOptions{},
		chExit:  make(chan struct{}, 1),
	}
	for _, opt := range opts {
		opt(h.options)
	}
	return h
}

func (h *tunHandler) Init(options ...HandlerOption) {
	if h.options == nil {
		h.options = &HandlerOptions{}
	}
	for _, opt := range options {
		opt(h.options)
	}
}

func (h *tunHandler) Handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	var err error
	var raddr net.Addr
	if addr := h.options.Node.Remote; addr != "" {
		raddr, err = net.ResolveUDPAddr("udp", addr)
		if err != nil {
			log.Debugf("[tun] %s: remote addr: %v", conn.LocalAddr(), err)
			return
		}
	}

	var tempDelay time.Duration
	for ctx.Err() == nil {
		err = func() error {
			var err error
			var pc net.PacketConn
			if raddr != nil && !h.options.Chain.IsEmpty() {
				cc, err := h.options.Chain.DialContext(ctx, "udp", raddr.String())
				if err != nil {
					return err
				}
				var ok bool
				pc, ok = cc.(net.PacketConn)
				if !ok {
					err = errors.New("not a packet connection")
					log.Debugf("[tun] %s - %s: %s", conn.LocalAddr(), raddr, err)
					return err
				}
			} else {
				laddr, _ := net.ResolveUDPAddr("udp", h.options.Node.Addr)
				pc, err = net.ListenUDP("udp", laddr)
			}
			if err != nil {
				return err
			}

			return h.transportTun(ctx, conn, pc, raddr)
		}()
		if err != nil {
			log.Debugf("[tun] %s: %v", conn.LocalAddr(), err)
		}

		select {
		case <-h.chExit:
			return
		case <-ctx.Done():
			h.chExit <- struct{}{}
		default:
			fmt.Println("next loop")
		}

		if err != nil {
			if tempDelay == 0 {
				tempDelay = 1000 * time.Millisecond
			} else {
				tempDelay *= 2
			}
			if max := 6 * time.Second; tempDelay > max {
				tempDelay = max
			}
			time.Sleep(tempDelay)
			continue
		}
		tempDelay = 0
	}
}

func (h *tunHandler) findRouteFor(dst net.IP) net.Addr {
	if v, ok := h.routes.Load(ipToTunRouteKey(dst)); ok {
		return v.(net.Addr)
	}
	for _, route := range h.options.IPRoutes {
		if route.Dest.Contains(dst) && route.Gateway != nil {
			if v, ok := h.routes.Load(ipToTunRouteKey(route.Gateway)); ok {
				return v.(net.Addr)
			}
		}
	}
	return nil
}

func (h *tunHandler) transportTun(ctx context.Context, tun net.Conn, conn net.PacketConn, raddr net.Addr) error {
	errChan := make(chan error, 2)
	defer func() {
		if c, ok := conn.(interface{ CloseRead() error }); ok {
			_ = c.CloseRead()
		}
		if c, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
		_ = conn.Close()
	}()
	go func() {
		for ctx.Err() == nil {
			err := func() error {
				b := util.SPool.Get().([]byte)
				defer util.SPool.Put(b)

				n, err := tun.Read(b)
				if err != nil {
					select {
					case h.chExit <- struct{}{}:
					default:
					}
					return err
				}

				var src, dst net.IP
				if waterutil.IsIPv4(b[:n]) {
					header, err := ipv4.ParseHeader(b[:n])
					if err != nil {
						log.Debugf("[tun] %s: %v", tun.LocalAddr(), err)
						return nil
					}
					if util.Debug {
						log.Debugf("[tun] %s", header.String())
					}
					src, dst = header.Src, header.Dst
				} else if waterutil.IsIPv6(b[:n]) {
					header, err := ipv6.ParseHeader(b[:n])
					if err != nil {
						log.Debugf("[tun] %s: %v", tun.LocalAddr(), err)
						return nil
					}
					if util.Debug {
						log.Debugf("[tun] %s", header.String())
					}
					src, dst = header.Src, header.Dst
				} else {
					log.Debugf("[tun] unknown packet")
					return nil
				}

				// client side, deliver packet directly.
				if raddr != nil {
					_, err := conn.WriteTo(b[:n], raddr)
					return err
				}

				addr := h.findRouteFor(dst)
				if addr == nil {
					log.Debugf("[tun] no route for %s -> %s", src, dst)
					return nil
				}

				if util.Debug {
					log.Debugf("[tun] find route: %s -> %s", dst, addr)
				}
				if _, err := conn.WriteTo(b[:n], addr); err != nil {
					return err
				}
				return nil
			}()

			if err != nil {
				errChan <- err
				return
			}
		}
	}()

	go func() {
		for ctx.Err() == nil {
			err := func() error {
				b := util.SPool.Get().([]byte)
				defer util.SPool.Put(b)

				n, addr, err := conn.ReadFrom(b)
				if err != nil && err != shadowaead.ErrShortPacket {
					return err
				}

				var src, dst net.IP
				if waterutil.IsIPv4(b[:n]) {
					header, err := ipv4.ParseHeader(b[:n])
					if err != nil {
						log.Debugf("[tun] %s: %v", tun.LocalAddr(), err)
						return nil
					}
					if util.Debug {
						log.Debugf("[tun] %s", header.String())
					}
					src, dst = header.Src, header.Dst
				} else if waterutil.IsIPv6(b[:n]) {
					header, err := ipv6.ParseHeader(b[:n])
					if err != nil {
						log.Debugf("[tun] %s: %v", tun.LocalAddr(), err)
						return nil
					}
					if util.Debug {
						log.Debugf("[tun] %s", header.String())
					}
					src, dst = header.Src, header.Dst
				} else {
					log.Debugf("[tun] unknown packet")
					return nil
				}

				// client side, deliver packet to tun device.
				if raddr != nil {
					_, err := tun.Write(b[:n])
					return err
				}

				routeKey := ipToTunRouteKey(src)
				if actual, loaded := h.routes.LoadOrStore(routeKey, addr); loaded {
					if actual.(net.Addr).String() != addr.String() {
						log.Debugf("[tun] update route: %s -> %s (old %s)", src, addr, actual.(net.Addr))
						h.routes.Store(routeKey, addr)
					}
				} else {
					log.Debugf("[tun] new route: %s -> %s", src, addr)
				}

				if addr := h.findRouteFor(dst); addr != nil {
					if util.Debug {
						log.Debugf("[tun] find route: %s -> %s", dst, addr)
					}
					_, err := conn.WriteTo(b[:n], addr)
					return err
				}

				if _, err := tun.Write(b[:n]); err != nil {
					select {
					case h.chExit <- struct{}{}:
					default:
					}
					return err
				}
				return nil
			}()

			if err != nil {
				errChan <- err
				return
			}
		}
	}()

	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		return nil
	}
}
