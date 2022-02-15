//go:build !amd64 && !arm64 && !x86 && !386
// +build !amd64,!arm64,!x86,!386

package pkg

import (
	"golang.org/x/net/route"
	"net"
	"syscall"
)

// not contains route like 10.61.64/18 10.61.64.1 UCS utun3, todo how about pull a merge to golang sdk???
// just contains 10.61.64, which mask is 0, 8, 16, 32, do not middle number
func getRouteTableByFetchRIB() (map[string][]*net.IPNet, error) {
	rib, err := route.FetchRIB(syscall.AF_UNSPEC, syscall.NET_RT_DUMP, 0)
	if err != nil {
		return nil, err
	}
	msgs, err := route.ParseRIB(syscall.NET_RT_DUMP, rib)
	if err != nil {
		return nil, err
	}
	nameToIndex := make(map[int]string)
	addrs, err := net.Interfaces()
	for _, addr := range addrs {
		nameToIndex[addr.Index] = addr.Name
	}

	m := make(map[string][]route.Addr)
	for _, msg := range msgs {
		message := msg.(*route.RouteMessage)
		if name, found := nameToIndex[message.Index]; found {
			if v, ok := m[name]; ok {
				temp := removeNilElement(message)
				m[name] = append(v, temp...)
			} else {
				m[name] = removeNilElement(message)
			}
		}
	}
	var mm = make(map[string][]*net.IPNet)
	for k, v := range m {
		var temp []*net.IPNet
		for _, addr := range v {
			if a, ok := addr.(*route.Inet4Addr); ok {
				var ip = make(net.IP, net.IPv4len)
				copy(ip, a.IP[:])
				if one, _ := mask(ip).Size(); one == 0 {
					continue
				}
				temp = append(temp, &net.IPNet{IP: ip, Mask: mask(ip)})
			} else if a, ok := addr.(*route.Inet6Addr); ok {
				var ip = make(net.IP, net.IPv6len)
				copy(ip, a.IP[:])
				if one, _ := mask(ip).Size(); one == 0 {
					continue
				}
				temp = append(temp, &net.IPNet{IP: ip, Mask: mask(ip)})
			} else if _, ok := addr.(*route.LinkAddr); ok {
				//fmt.Println(a)
				// todo
			}
		}
		mm[k] = temp
	}
	return mm, nil
}

func removeNilElement(message *route.RouteMessage) []route.Addr {
	var temp []route.Addr
	for _, addr := range message.Addrs {
		if addr != nil {
			temp = append(temp, addr)
		}
	}
	return temp
}

// route.FetchRIB fetches a routing information base from the operating system.
// In most cases, zero means a wildcard.
func mask(ip []byte) net.IPMask {
	size := 0
	for i := len(ip) - 1; i >= 0; i-- {
		if ip[i] == 0 {
			size++
		} else {
			break
		}
	}
	// 0.0.0.0
	if size == len(ip) {
		return net.CIDRMask(len(ip)*8, len(ip)*8)
	}
	if size == 0 {
		return net.CIDRMask(0, len(ip)*8)
	}
	return net.CIDRMask((len(ip)-size)*8, len(ip)*8)
}
