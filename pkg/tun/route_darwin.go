package tun

import (
	"errors"
	"net"
	"net/netip"
	"os"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

func addRoute(seq int, r netip.Prefix, gw route.Addr) error {
	return withRouteSocket(func(routeSocket int) error {
		m := newRouteMessage(unix.RTM_ADD, seq, r, gw)
		rb, err := m.Marshal()
		if err != nil {
			return err
		}
		_, err = unix.Write(routeSocket, rb)
		if errors.Is(err, unix.EEXIST) {
			err = nil
		}
		return err
	})
}

func deleteRoute(seq int, r netip.Prefix, gw route.Addr) error {
	return withRouteSocket(func(routeSocket int) error {
		m := newRouteMessage(unix.RTM_DELETE, seq, r, gw)
		rb, err := m.Marshal()
		if err != nil {
			return err
		}
		_, err = unix.Write(routeSocket, rb)
		if errors.Is(err, unix.ESRCH) {
			err = nil
		}
		return err
	})
}

func withRouteSocket(f func(routeSocket int) error) error {
	routeSocket, err := unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, unix.AF_UNSPEC)
	if err != nil {
		return err
	}

	// Avoid the overhead of echoing messages back to sender
	if err = unix.SetsockoptInt(routeSocket, unix.SOL_SOCKET, unix.SO_USELOOPBACK, 0); err != nil {
		return err
	}
	defer unix.Close(routeSocket)
	return f(routeSocket)
}

func newRouteMessage(rtm, seq int, subnet netip.Prefix, gw route.Addr) *route.RouteMessage {
	var mask, dst route.Addr
	if subnet.Addr().Is4() {
		mask = &route.Inet4Addr{IP: [4]byte(net.CIDRMask(subnet.Bits(), 32))}
		dst = &route.Inet4Addr{IP: subnet.Addr().As4()}
	} else {
		mask = &route.Inet6Addr{IP: [16]byte(net.CIDRMask(subnet.Bits(), 128))}
		dst = &route.Inet6Addr{IP: subnet.Addr().As16()}
	}
	return &route.RouteMessage{
		Version: unix.RTM_VERSION,
		ID:      uintptr(os.Getpid()),
		Seq:     seq,
		Type:    rtm,
		Flags:   unix.RTF_UP | unix.RTF_STATIC | unix.RTF_CLONING | unix.RTF_GATEWAY,
		Addrs: []route.Addr{
			unix.RTAX_DST:     dst,
			unix.RTAX_GATEWAY: gw,
			unix.RTAX_NETMASK: mask,
		},
	}
}
