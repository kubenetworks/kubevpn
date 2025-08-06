package util

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cilium/ipam/service/allocator"
	"github.com/cilium/ipam/service/ipallocator"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/prometheus-community/pro-bing"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func GetTunDevice(ips ...net.IP) (*net.Interface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, i := range interfaces {
		addrList, err := i.Addrs()
		if err != nil {
			return nil, err
		}
		for _, addr := range addrList {
			if ipNet, ok := addr.(*net.IPNet); ok {
				for _, ip := range ips {
					if ipNet.IP.Equal(ip) {
						return &i, nil
					}
				}
			}
		}
	}
	return nil, fmt.Errorf("can not found any interface with IP %v", ips)
}

func GetTunDeviceByConn(tun net.Conn) (*net.Interface, error) {
	var ip net.IP
	switch tun.LocalAddr().(type) {
	case *net.IPNet:
		ip = tun.LocalAddr().(*net.IPNet).IP
	case *net.IPAddr:
		ip = tun.LocalAddr().(*net.IPAddr).IP
	}
	return GetTunDevice(ip)
}

func GetTunDeviceIP(tunName string) (net.IP, net.IP, net.IP, error) {
	tunIfi, err := net.InterfaceByName(tunName)
	if err != nil {
		return nil, nil, nil, err
	}
	addrList, err := tunIfi.Addrs()
	if err != nil {
		return nil, nil, nil, err
	}
	var srcIPv4, srcIPv6, dockerSrcIPv4 net.IP
	for _, addr := range addrList {
		if ipNet, ok := addr.(*net.IPNet); ok {
			if config.CIDR.Contains(ipNet.IP) {
				srcIPv4 = ipNet.IP
			}
			if config.CIDR6.Contains(ipNet.IP) {
				srcIPv6 = ipNet.IP
			}
			if config.DockerCIDR.Contains(ipNet.IP) {
				dockerSrcIPv4 = ipNet.IP
			}
		}
	}
	return srcIPv4, srcIPv6, dockerSrcIPv4, nil
}

func Ping(ctx context.Context, srcIP, dstIP string) (bool, error) {
	pinger, err := probing.NewPinger(dstIP)
	if err != nil {
		return false, err
	}
	pinger.Source = srcIP
	pinger.SetLogger(nil)
	pinger.SetPrivileged(true)
	pinger.Count = 4
	pinger.Timeout = time.Second * 4
	pinger.ResolveTimeout = time.Second * 1
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

func ParseIP(packet []byte) (src net.IP, dst net.IP, protocol int, err error) {
	if IsIPv4(packet) {
		header, err := ipv4.ParseHeader(packet)
		if err != nil {
			return nil, nil, -1, err
		}
		return header.Src, header.Dst, header.Protocol, nil
	}
	if IsIPv6(packet) {
		header, err := ipv6.ParseHeader(packet)
		if err != nil {
			return nil, nil, -1, err
		}
		return header.Src, header.Dst, header.NextHeader, nil
	}
	return nil, nil, -1, errors.New("packet is invalid")
}

func GetIPBaseNic() (*net.IPNet, error) {
	addrs, _ := net.InterfaceAddrs()
	var sum int
	for _, addr := range addrs {
		for _, b := range getIP(addr) {
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

func getIP(addr net.Addr) net.IP {
	if addr == nil {
		return nil
	}

	var ip net.IP
	switch addr.(type) {
	case *net.IPAddr:
		ip = addr.(*net.IPAddr).IP
	case *net.IPNet:
		ip = addr.(*net.IPNet).IP
	default:
		ip = net.ParseIP(addr.String())
	}
	return ip
}

func GenICMPPacket(src net.IP, dst net.IP) ([]byte, error) {
	buf := gopacket.NewSerializeBuffer()
	var id uint16
	for _, b := range src {
		id += uint16(b)
	}
	icmpLayer := layers.ICMPv4{
		TypeCode: layers.CreateICMPv4TypeCode(layers.ICMPv4TypeEchoRequest, 0),
		Id:       id,
		Seq:      uint16(rand.Intn(math.MaxUint16 + 1)),
	}
	ipLayer := layers.IPv4{
		Version:  4,
		SrcIP:    src,
		DstIP:    dst,
		Protocol: layers.IPProtocolICMPv4,
		Flags:    layers.IPv4DontFragment,
		TTL:      64,
		IHL:      5,
		Id:       uint16(rand.Intn(math.MaxUint16 + 1)),
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

func GenICMPPacketIPv6(src net.IP, dst net.IP) ([]byte, error) {
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

func DetectSupportIPv6() (bool, error) {
	content, err := os.ReadFile("/proc/sys/net/ipv6/conf/all/disable_ipv6")
	if err != nil {
		return false, err
	}
	disableIPv6, err := strconv.Atoi(strings.TrimSpace(string(content)))
	if err != nil {
		return false, err
	}
	return disableIPv6 == 0, nil
}

func IsValidCIDR(str string) bool {
	_, _, err := net.ParseCIDR(str)
	if err != nil {
		return false
	}
	return true
}
