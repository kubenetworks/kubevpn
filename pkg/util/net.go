package util

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/cilium/ipam/service/allocator"
	"github.com/cilium/ipam/service/ipallocator"
	"github.com/prometheus-community/pro-bing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func GetTunDevice(ips ...net.IP) (*net.Interface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, i := range interfaces {
		addrs, err := i.Addrs()
		if err != nil {
			return nil, err
		}
		for _, addr := range addrs {
			for _, ip := range ips {
				if strings.Contains(addr.String(), ip.String()) {
					return &i, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("can not found any interface with ip %v", ips)
}

func GetTunDeviceByConn(tun net.Conn) (*net.Interface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var ip string
	if tunIP, ok := tun.LocalAddr().(*net.IPNet); ok {
		ip = tunIP.IP.String()
	} else {
		ip = tun.LocalAddr().String()
	}
	for _, i := range interfaces {
		addrs, err := i.Addrs()
		if err != nil {
			return nil, err
		}
		for _, addr := range addrs {
			if strings.Contains(addr.String(), tun.LocalAddr().String()) {
				return &i, nil
			}
		}
	}
	return nil, fmt.Errorf("can not found any interface with ip %v", ip)
}

func Ping(ctx context.Context, srcIP, dstIP string) (bool, error) {
	pinger, err := probing.NewPinger(dstIP)
	if err != nil {
		return false, err
	}
	pinger.Source = srcIP
	pinger.SetLogger(nil)
	pinger.SetPrivileged(true)
	pinger.Count = 3
	pinger.Timeout = time.Millisecond * 1500
	err = pinger.RunWithContext(ctx) // Blocks until finished.
	if err != nil {
		return false, err
	}
	stat := pinger.Statistics()
	return stat.PacketsRecv == stat.PacketsSent, err
}

func IsIPv4(packet []byte) bool {
	return 4 == (packet[0] >> 4)
}

func IsIPv6(packet []byte) bool {
	return 6 == (packet[0] >> 4)
}

func GetIPBaseNic() (*net.IPNet, error) {
	addrs, _ := net.InterfaceAddrs()
	var sum int
	for _, addr := range addrs {
		ip, _, _ := net.ParseCIDR(addr.String())
		for _, b := range ip {
			sum = sum + int(b)
		}
	}
	dhcp, err := ipallocator.NewAllocatorCIDRRange(config.DockerCIDR, func(max int, rangeSpec string) (allocator.Interface, error) {
		return allocator.NewContiguousAllocationMap(max, rangeSpec), nil
	})
	if err != nil {
		return nil, err
	}
	var next net.IP
	for i := 0; i < sum%255; i++ {
		next, err = dhcp.AllocateNext()
	}
	if err != nil {
		return nil, err
	}
	_, bits := config.DockerCIDR.Mask.Size()
	return &net.IPNet{IP: next, Mask: net.CIDRMask(bits, bits)}, nil
}
