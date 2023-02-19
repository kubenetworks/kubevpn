package core

import (
	"context"
	"errors"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"net"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func ipToTunRouteKey(ip net.IP) string {
	return ip.To16().String()
}

type tunHandler struct {
	chain  *Chain
	node   *Node
	routes *sync.Map
	chExit chan struct{}
}

// TunHandler creates a handler for tun tunnel.
func TunHandler(chain *Chain, node *Node) Handler {
	return &tunHandler{
		chain:  chain,
		node:   node,
		routes: &sync.Map{},
		chExit: make(chan struct{}, 1),
	}
}

func (h *tunHandler) Handle(ctx context.Context, tun net.Conn) {
	defer tun.Close()
	var err error
	var remoteAddr net.Addr
	if addr := h.node.Remote; addr != "" {
		remoteAddr, err = net.ResolveUDPAddr("udp", addr)
		if err != nil {
			log.Errorf("[tun] %s: remote addr: %v", tun.LocalAddr(), err)
			return
		}
	}

	for {
		err = func() error {
			var err error
			var packetConn net.PacketConn
			if remoteAddr != nil && !h.chain.IsEmpty() {
				cc, err := h.chain.DialContext(ctx)
				if err != nil {
					return err
				}
				var ok bool
				packetConn, ok = cc.(net.PacketConn)
				if !ok {
					err = errors.New("not a packet connection")
					log.Errorf("[tun] %s - %s: %s", tun.LocalAddr(), remoteAddr, err)
					return err
				}
			} else {
				var lc net.ListenConfig
				packetConn, err = lc.ListenPacket(ctx, "udp", h.node.Addr)
			}
			if err != nil {
				return err
			}

			return h.transportTun(ctx, tun, packetConn, remoteAddr)
		}()
		if err != nil {
			log.Debugf("[tun] %s: %v", tun.LocalAddr(), err)
		}

		select {
		case <-h.chExit:
		case <-ctx.Done():
			return
		default:
			log.Debugf("next loop, err: %v", err)
		}
	}
}

func (h *tunHandler) findRouteFor(dst net.IP) net.Addr {
	if v, ok := h.routes.Load(ipToTunRouteKey(dst)); ok {
		return v.(net.Addr)
	}
	//for _, route := range h.options.IPRoutes {
	//	if route.Dest.Contains(dst) && route.Gateway != nil {
	//		if v, ok := h.routes.Load(ipToTunRouteKey(route.Gateway)); ok {
	//			return v.(net.Addr)
	//		}
	//	}
	//}
	return nil
}

func (h *tunHandler) transportTun(ctx context.Context, tun net.Conn, conn net.PacketConn, remoteAddr net.Addr) error {
	errChan := make(chan error, 2)
	defer conn.Close()
	go func() {
		b := LPool.Get().([]byte)
		defer LPool.Put(b)

		for {
			err := func() error {
				n, err := tun.Read(b[:])
				if err != nil {
					select {
					case h.chExit <- struct{}{}:
					default:
					}
					return err
				}

				// client side, deliver packet directly.
				if remoteAddr != nil {
					_, err = conn.WriteTo(b[:n], remoteAddr)
					return err
				}

				var src, dst net.IP
				if util.IsIPv4(b[:n]) {
					header, err := ipv4.ParseHeader(b[:n])
					if err != nil {
						log.Errorf("[tun] %s: %v", tun.LocalAddr(), err)
						return nil
					}
					log.Debugf("[tun] %s", header.String())
					src, dst = header.Src, header.Dst
				} else if util.IsIPv6(b[:n]) {
					header, err := ipv6.ParseHeader(b[:n])
					if err != nil {
						log.Errorf("[tun] %s: %v", tun.LocalAddr(), err)
						return nil
					}
					log.Debugf("[tun] %s", header.String())
					src, dst = header.Src, header.Dst
				} else {
					log.Errorf("[tun] unknown packet")
					return nil
				}

				addr := h.findRouteFor(dst)
				if addr == nil {
					log.Debugf("[tun] no route for %s -> %s", src, dst)
					return nil
				}

				log.Debugf("[tun] find route: %s -> %s", dst, addr)
				_, err = conn.WriteTo(b[:n], addr)
				return err
			}()

			if err != nil {
				select {
				case errChan <- err:
				default:
				}
				return
			}
		}
	}()

	go func() {
		b := LPool.Get().([]byte)
		defer LPool.Put(b)

		for {
			err := func() error {
				n, srcAddr, err := conn.ReadFrom(b[:])
				if err != nil {
					return err
				}

				// client side, deliver packet to tun device.
				if remoteAddr != nil {
					_, err = tun.Write(b[:n])
					return err
				}

				var src, dst net.IP
				if util.IsIPv4(b[:n]) {
					header, err := ipv4.ParseHeader(b[:n])
					if err != nil {
						log.Errorf("[tun] %s: %v", tun.LocalAddr(), err)
						return nil
					}
					log.Debugf("[tun] %s", header.String())
					src, dst = header.Src, header.Dst
				} else if util.IsIPv6(b[:n]) {
					header, err := ipv6.ParseHeader(b[:n])
					if err != nil {
						log.Errorf("[tun] %s: %v", tun.LocalAddr(), err)
						return nil
					}
					log.Debugf("[tun] %s", header.String())
					src, dst = header.Src, header.Dst
				} else {
					log.Errorf("[tun] unknown packet")
					return nil
				}

				routeKey := ipToTunRouteKey(src)
				if actual, loaded := h.routes.LoadOrStore(routeKey, srcAddr); loaded {
					if actual.(net.Addr).String() != srcAddr.String() {
						log.Debugf("[tun] update route: %s -> %s (old %s)", src, srcAddr, actual.(net.Addr))
						h.routes.Store(routeKey, srcAddr)
					}
				} else {
					log.Debugf("[tun] new route: %s -> %s", src, srcAddr)
				}

				if routeToAddr := h.findRouteFor(dst); routeToAddr != nil {
					log.Debugf("[tun] find route: %s -> %s", dst, routeToAddr)
					_, err = conn.WriteTo(b[:n], routeToAddr)
					return err
				}

				if _, err = tun.Write(b[:n]); err != nil {
					select {
					case h.chExit <- struct{}{}:
					default:
					}
					return err
				}
				return nil
			}()

			if err != nil {
				select {
				case errChan <- err:
				default:
				}
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
