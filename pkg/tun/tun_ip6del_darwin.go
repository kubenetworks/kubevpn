//go:build darwin

package tun

import (
	"net/netip"
	"unsafe"

	"golang.org/x/sys/unix"
)

// macOS: SIOCDIFADDR_IN6 = _IOW('i', 25, struct in6_ifreq). struct in6_ifreq is
// ifr_name[IFNAMSIZ] plus a union whose largest member (struct icmp6_ifstat) makes the
// whole struct 288 bytes; the ioctl request number embeds that size. We size in6Ifreq to
// match and recompute the number the same way setInet6Address derives siocAIFAddrInet6 for
// the add path, so the delete is a pure syscall (no ifconfig). Only the ifr_name and the
// sockaddr_in6 at the head are read by the kernel to locate the address; the rest is padding
// present solely so unsafe.Sizeof yields the kernel's in6_ifreq size.
const sizeofIn6Ifreq = 288

type in6Ifreq struct {
	name [unix.IFNAMSIZ]byte
	addr unix.RawSockaddrInet6
	_    [sizeofIn6Ifreq - unix.IFNAMSIZ - unix.SizeofSockaddrInet6]byte
}

// siocDIFAddrInet6 == 0x81206919 on macOS: take SIOCDIFADDR's direction bits + group/number
// ('i', 25) and OR in sizeof(in6_ifreq).
const siocDIFAddrInet6 = (unix.SIOCDIFADDR & 0xe000ffff) | (uint(unsafe.Sizeof(in6Ifreq{})) << 16)

func delInet6Address(ifName string, ip netip.Addr) error {
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	req := &in6Ifreq{
		addr: unix.RawSockaddrInet6{Family: unix.AF_INET6, Len: unix.SizeofSockaddrInet6, Addr: ip.As16()},
	}
	copy(req.name[:], ifName)
	return ioctlRequest(fd, siocDIFAddrInet6, unsafe.Pointer(req))
}
