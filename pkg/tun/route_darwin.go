package tun

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"

	"github.com/containernetworking/cni/pkg/types"
	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

func addTunRoutes(ifName string, routes ...types.Route) error {
	tunIfi, err := net.InterfaceByName(ifName)
	if err != nil {
		return err
	}
	gw := &route.LinkAddr{Index: tunIfi.Index}

	var prefixList []netip.Prefix
	for _, r := range routes {
		if net.ParseIP(r.Dst.IP.String()) == nil {
			continue
		}
		var prefix netip.Prefix
		prefix, err = netip.ParsePrefix(r.Dst.String())
		if err != nil {
			return err
		}
		prefixList = append(prefixList, prefix)
	}
	if len(prefixList) == 0 {
		return nil
	}
	err = addRoute(gw, prefixList...)
	if err != nil {
		return fmt.Errorf("failed to add dst %v via %s to route table: %v", prefixList, ifName, err)
	}
	return nil
}

func deleteTunRoutes(ifName string, routes ...types.Route) error {
	tunIfi, err := net.InterfaceByName(ifName)
	if err != nil {
		return err
	}
	gw := &route.LinkAddr{Index: tunIfi.Index}

	var prefixList []netip.Prefix
	for _, r := range routes {
		if net.ParseIP(r.Dst.IP.String()) == nil {
			continue
		}
		var prefix netip.Prefix
		prefix, err = netip.ParsePrefix(r.Dst.String())
		if err != nil {
			return err
		}
		prefixList = append(prefixList, prefix)
	}
	if len(prefixList) == 0 {
		return nil
	}
	err = deleteRoute(gw, prefixList...)
	if err != nil {
		return fmt.Errorf("failed to add dst %v via %s to route table: %v", prefixList, ifName, err)
	}
	return nil
}

func addRoute(gw route.Addr, r ...netip.Prefix) error {
	if len(r) == 0 {
		return nil
	}
	return withRouteSocket(func(routeSocket int) error {
		for i, prefix := range r {
			m := newRouteMessage(unix.RTM_ADD, i+1, prefix, gw)
			rb, err := m.Marshal()
			if err != nil {
				return err
			}
			_, err = unix.Write(routeSocket, rb)
			if errors.Is(err, unix.EEXIST) {
				err = nil
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func deleteRoute(gw route.Addr, r ...netip.Prefix) error {
	if len(r) == 0 {
		return nil
	}
	return withRouteSocket(func(routeSocket int) error {
		for i, prefix := range r {
			m := newRouteMessage(unix.RTM_DELETE, i+1, prefix, gw)
			rb, err := m.Marshal()
			if err != nil {
				return err
			}
			_, err = unix.Write(routeSocket, rb)
			if errors.Is(err, unix.ESRCH) {
				err = nil
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func withRouteSocket(f func(routeSocket int) error) error {
	routeSocket, err := unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, unix.AF_UNSPEC)
	if err != nil {
		return err
	}
	defer unix.Close(routeSocket)
	// Avoid the overhead of echoing messages back to sender
	if err = unix.SetsockoptInt(routeSocket, unix.SOL_SOCKET, unix.SO_USELOOPBACK, 0); err != nil {
		return err
	}
	// Set receive buffer size 1024k
	if err = unix.SetsockoptInt(routeSocket, unix.SOL_SOCKET, unix.SO_RCVBUF, 1024*1024); err != nil {
		return err
	}
	// Set send buffer size 1024k
	if err = unix.SetsockoptInt(routeSocket, unix.SOL_SOCKET, unix.SO_SNDBUF, 1024*1024); err != nil {
		return err
	}
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
