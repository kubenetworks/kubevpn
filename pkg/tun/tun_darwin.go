//go:build darwin || freebsd || openbsd

package tun

import (
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"unsafe"

	pkgerr "github.com/pkg/errors"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/tun"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func createTun(cfg Config) (conn net.Conn, itf *net.Interface, err error) {
	if cfg.Addr == "" && cfg.Addr6 == "" {
		err = fmt.Errorf("IPv4 address and IPv6 address can not be empty at same time")
		return
	}

	var ipv4, ipv6 net.IP

	mtu := cfg.MTU
	if mtu <= 0 {
		mtu = config.DefaultMTU
	}

	var ifce tun.Device
	ifce, err = tun.CreateTUN("utun", mtu)
	if err != nil {
		return
	}

	var name string
	name, err = ifce.Name()
	if err != nil {
		return
	}

	// set ipv4 address
	if cfg.Addr != "" {
		if ipv4, _, err = net.ParseCIDR(cfg.Addr); err != nil {
			return
		}
		var prefix netip.Prefix
		prefix, err = netip.ParsePrefix(cfg.Addr)
		if err != nil {
			return
		}
		err = setInterfaceAddress(name, prefix)
		if err != nil {
			return
		}
	}

	// set ipv6 address
	if cfg.Addr6 != "" {
		if ipv6, _, err = net.ParseCIDR(cfg.Addr6); err != nil {
			return
		}
		var prefix netip.Prefix
		prefix, err = netip.ParsePrefix(cfg.Addr6)
		if err != nil {
			return
		}
		err = setInterfaceAddress(name, prefix)
		if err != nil {
			return
		}
	}

	if err = addTunRoutes(name, cfg.Routes...); err != nil {
		err = pkgerr.Wrap(err, "Add tun device routes failed")
		return
	}

	if itf, err = net.InterfaceByName(name); err != nil {
		return
	}

	conn = &tunConn{
		ifce:  ifce,
		addr:  &net.IPAddr{IP: ipv4},
		addr6: &net.IPAddr{IP: ipv6},
	}
	return
}

// struct ifaliasreq
type inet4AliasReq struct {
	name    [unix.IFNAMSIZ]byte
	addr    unix.RawSockaddrInet4
	dstaddr unix.RawSockaddrInet4
	mask    unix.RawSockaddrInet4
}

// ifaliasreq
type inet6AliasReq struct {
	name     [unix.IFNAMSIZ]byte
	addr     unix.RawSockaddrInet6
	dstaddr  unix.RawSockaddrInet6
	mask     unix.RawSockaddrInet6
	flags    int32
	lifetime struct {
		expire    float64
		preferred float64
		valid     uint32
		pref      uint32
	}
}

// IPv6 SIOCAIFADDR
const (
	siocAIFAddrInet6 = (unix.SIOCAIFADDR & 0xe000ffff) | (uint(unsafe.Sizeof(inet6AliasReq{})) << 16)
	v6InfiniteLife   = 0xffffffff
	v6IfaceFlags     = 0x0020 | 0x0400 // NODAD + SECURED
)

func setInterfaceAddress(ifName string, addr netip.Prefix) error {
	ip := addr.Addr()
	if ip.Is4() {
		return setInet4Address(ifName, addr)
	}
	return setInet6Address(ifName, addr)
}

func setInet4Address(ifName string, prefix netip.Prefix) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ip4 := prefix.Addr().As4()
	req := &inet4AliasReq{
		addr:    unix.RawSockaddrInet4{Family: unix.AF_INET, Len: unix.SizeofSockaddrInet4, Addr: ip4},
		dstaddr: unix.RawSockaddrInet4{Family: unix.AF_INET, Len: unix.SizeofSockaddrInet4, Addr: ip4},
		mask:    unix.RawSockaddrInet4{Family: unix.AF_INET, Len: unix.SizeofSockaddrInet4, Addr: [4]byte(net.CIDRMask(prefix.Bits(), 32))},
	}
	copy(req.name[:], ifName)

	return ioctlRequest(fd, unix.SIOCAIFADDR, unsafe.Pointer(req))
}

func setInet6Address(ifName string, prefix netip.Prefix) error {
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	ip6 := prefix.Addr().As16()
	req := &inet6AliasReq{
		addr:    unix.RawSockaddrInet6{Family: unix.AF_INET6, Len: unix.SizeofSockaddrInet6, Addr: ip6},
		dstaddr: unix.RawSockaddrInet6{Family: unix.AF_INET6, Len: unix.SizeofSockaddrInet6, Addr: ip6},
		mask:    unix.RawSockaddrInet6{Family: unix.AF_INET6, Len: unix.SizeofSockaddrInet6, Addr: [16]byte(net.CIDRMask(prefix.Bits(), 128))},
		flags:   v6IfaceFlags,
	}
	copy(req.name[:], ifName)
	req.lifetime.valid = v6InfiniteLife
	req.lifetime.pref = v6InfiniteLife

	return ioctlRequest(fd, siocAIFAddrInet6, unsafe.Pointer(req))
}

func ioctlRequest(fd int, req uint, ptr unsafe.Pointer) error {
	err := unix.IoctlSetInt(fd, req, int(uintptr(ptr)))
	runtime.KeepAlive(ptr)
	if err != nil {
		return err
	}
	return nil
}
