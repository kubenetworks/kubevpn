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
		return fmt.Errorf("failed to add dst %v via %s to route table: %w", prefixList, ifName, err)
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
		return fmt.Errorf("failed to delete dst %v from route table via %s: %w", prefixList, ifName, err)
	}
	return nil
}

// addRoute installs each prefix pointing at gw, re-asserting one that is already present
// instead of leaving it alone. The kernel pins RTAX_IFA — the source address outbound packets
// matching this route will carry — when the entry is created, so an entry installed before a
// TUN IP change keeps sourcing from an address that no longer exists on the device. RTM_CHANGE
// makes the kernel re-derive the ifa from the gateway, which is the only way to correct that
// in place; treating EEXIST as success (as this did) made the stale source permanent.
// See docs/52-stale-route-src-after-ip-change.md.
// listTunRoutes returns the routes the kernel currently has attached to ifName that kubevpn
// installed, identified by RTF_STATIC — the flag newRouteMessage always sets. The scope is
// deliberately the kernel's table rather than a list of "routes we remember adding": entries
// reach a TUN device from four places (device creation in tun_darwin.go, NetworkManager's
// AddRoute and AddRouteCIDR, and the `kubevpn route add` RPC in daemon/action/route.go), and
// a remembered list silently stops covering them the moment a fifth appears.
//
// Interface-scope entries the kernel derives from the device's own address carry no RTF_STATIC
// and are skipped: they belong to the address, and re-asserting them would fight SIOCAIFADDR.
func listTunRoutes(ifName string) ([]types.Route, error) {
	tunIfi, err := net.InterfaceByName(ifName)
	if err != nil {
		return nil, err
	}
	rib, err := route.FetchRIB(unix.AF_UNSPEC, route.RIBTypeRoute, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch routing table: %w", err)
	}
	msgs, err := route.ParseRIB(route.RIBTypeRoute, rib)
	if err != nil {
		return nil, fmt.Errorf("failed to parse routing table: %w", err)
	}
	var routes []types.Route
	for _, msg := range msgs {
		m, ok := msg.(*route.RouteMessage)
		if !ok || m.Index != tunIfi.Index || m.Flags&unix.RTF_STATIC == 0 {
			continue
		}
		dst, ok := ipFromRouteAddr(routeAddrAt(m.Addrs, unix.RTAX_DST))
		if !ok {
			continue
		}
		routes = append(routes, types.Route{Dst: net.IPNet{
			IP:   dst,
			Mask: maskFromRouteAddr(routeAddrAt(m.Addrs, unix.RTAX_NETMASK), len(dst)),
		}})
	}
	return routes, nil
}

// routeAddrAt indexes the sparse RTAX_* address slice of a routing message, which is only as
// long as the highest address actually present.
func routeAddrAt(addrs []route.Addr, rtax int) route.Addr {
	if rtax >= len(addrs) {
		return nil
	}
	return addrs[rtax]
}

// ipFromRouteAddr copies the address out of a routing message. The copy matters: the parsed
// Addr aliases the RIB buffer, which the caller is free to reuse.
func ipFromRouteAddr(a route.Addr) (net.IP, bool) {
	switch v := a.(type) {
	case *route.Inet4Addr:
		return net.IP(append([]byte(nil), v.IP[:]...)), true
	case *route.Inet6Addr:
		return net.IP(append([]byte(nil), v.IP[:]...)), true
	}
	return nil, false
}

// maskFromRouteAddr turns RTAX_NETMASK into an IPMask of ipLen bytes. A host route carries no
// netmask at all, which is why the fallback is an all-ones mask rather than an empty one.
func maskFromRouteAddr(a route.Addr, ipLen int) net.IPMask {
	switch v := a.(type) {
	case *route.Inet4Addr:
		return net.IPMask(append([]byte(nil), v.IP[:]...))
	case *route.Inet6Addr:
		return net.IPMask(append([]byte(nil), v.IP[:]...))
	}
	return net.CIDRMask(ipLen*8, ipLen*8)
}

func addRoute(gw route.Addr, r ...netip.Prefix) error {
	if len(r) == 0 {
		return nil
	}
	return withRouteSocket(func(routeSocket int) error {
		for i, prefix := range r {
			err := writeRouteMessage(routeSocket, unix.RTM_ADD, i+1, prefix, gw)
			if errors.Is(err, unix.EEXIST) {
				err = writeRouteMessage(routeSocket, unix.RTM_CHANGE, i+1, prefix, gw)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// writeRouteMessage marshals and writes a single routing-socket message, returning the raw
// syscall error so callers can branch on EEXIST / ESRCH.
func writeRouteMessage(routeSocket, rtm, seq int, prefix netip.Prefix, gw route.Addr) error {
	rb, err := newRouteMessage(rtm, seq, prefix, gw).Marshal()
	if err != nil {
		return err
	}
	_, err = unix.Write(routeSocket, rb)
	return err
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

// routeSocketBufferSize is the send/receive buffer size (1024k) set on the route socket.
const routeSocketBufferSize = 1024 * 1024

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
	if err = unix.SetsockoptInt(routeSocket, unix.SOL_SOCKET, unix.SO_RCVBUF, routeSocketBufferSize); err != nil {
		return err
	}
	// Set send buffer size 1024k
	if err = unix.SetsockoptInt(routeSocket, unix.SOL_SOCKET, unix.SO_SNDBUF, routeSocketBufferSize); err != nil {
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
