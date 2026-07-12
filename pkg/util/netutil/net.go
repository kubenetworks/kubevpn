package netutil

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

// GetTunDevice returns the network interface that has one of the specified IPs assigned.
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
	return nil, fmt.Errorf("cannot find any interface with IP %v", ips)
}

// GetTunDeviceByConn returns the network interface associated with the local address of the TUN connection.
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

// GetTunDeviceIP returns the IPv4, IPv6, and Docker IPv4 addresses assigned to the named TUN interface.
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

// Ping sends ICMP echo requests from srcIP to dstIP and reports whether all packets were received.
func Ping(ctx context.Context, srcIP, dstIP string) (bool, error) {
	pinger, err := probing.NewPinger(dstIP)
	if err != nil {
		return false, err
	}
	pinger.Source = srcIP
	const (
		pingCount          = 4
		pingTimeout        = 4 * time.Second
		pingResolveTimeout = 1 * time.Second
	)
	pinger.SetLogger(nil)
	pinger.SetPrivileged(true)
	pinger.Count = pingCount
	pinger.Timeout = pingTimeout
	pinger.ResolveTimeout = pingResolveTimeout
	err = pinger.RunWithContext(ctx) // Blocks until finished.
	if err != nil {
		return false, err
	}
	stat := pinger.Statistics()
	return stat.PacketsRecv == stat.PacketsSent, err
}

// IsIPv4 checks if the packet starts with IPv4 version nibble.
func IsIPv4(packet []byte) bool {
	return len(packet) > 0 && (packet[0]>>4) == 4
}

// IsIPv6 checks if the packet starts with IPv6 version nibble.
func IsIPv6(packet []byte) bool {
	return len(packet) > 0 && (packet[0]>>4) == 6
}

const (
	icmpv4ProtocolNumber = 1
	icmpv6ProtocolNumber = 58
	icmpv4EchoReplyType  = 0
	icmpv6EchoReplyType  = 129
)

// IsICMPEchoReplyFrom reports whether packet is an ICMP/ICMPv6 Echo Reply whose source
// address equals src. It is a lightweight, allocation-free parse for the inbound hot path
// (no IPv6 extension headers are expected for gateway heartbeat replies).
func IsICMPEchoReplyFrom(packet []byte, src net.IP) bool {
	if len(packet) < 1 || src == nil {
		return false
	}
	switch {
	case IsIPv4(packet):
		ihl := int(packet[0]&0x0f) * 4
		if len(packet) < ihl+1 || ihl < 20 {
			return false
		}
		if packet[9] != icmpv4ProtocolNumber {
			return false
		}
		if packet[ihl] != icmpv4EchoReplyType {
			return false
		}
		return src.Equal(net.IP(packet[12:16]))
	case IsIPv6(packet):
		const ipv6HeaderLen = 40
		if len(packet) < ipv6HeaderLen+1 {
			return false
		}
		if packet[6] != icmpv6ProtocolNumber {
			return false
		}
		if packet[ipv6HeaderLen] != icmpv6EchoReplyType {
			return false
		}
		return src.Equal(net.IP(packet[8:24]))
	default:
		return false
	}
}

// ParseIP extracts the source IP, destination IP, and protocol number from a raw IPv4 or IPv6 packet.
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

// GetLocalIPNet allocates a deterministic IP from the Docker CIDR based on a hash of local interface addresses.
func GetLocalIPNet() (*net.IPNet, error) {
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
	// Allocate at least once so the returned IP is never nil (sum%255 can be 0).
	count := sum % 255
	if count == 0 {
		count = 1
	}
	var next net.IP
	for i := 0; i < count; i++ {
		next, err = dhcp.AllocateNext()
		if err != nil {
			return nil, err
		}
	}
	_, bits := config.DockerCIDR.Mask.Size()
	return &net.IPNet{IP: next, Mask: net.CIDRMask(bits, bits)}, nil
}

func getIP(addr net.Addr) net.IP {
	if addr == nil {
		return nil
	}

	var ip net.IP
	switch a := addr.(type) {
	case *net.IPAddr:
		ip = a.IP
	case *net.IPNet:
		ip = a.IP
	default:
		ip = net.ParseIP(addr.String())
	}
	return ip
}

// GenICMPPacket generates a minimal IPv4 ICMP Echo Request packet.
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
		return nil, fmt.Errorf("failed to serialize icmp packet: %w", err)
	}
	return buf.Bytes(), nil
}

// GenICMPPacket generates a minimal IPv4 ICMP Echo Request packet.
// GenICMPPacketIPv6 generates a minimal IPv6 ICMPv6 Echo Request packet.
func GenICMPPacketIPv6(src net.IP, dst net.IP) ([]byte, error) {
	buf := gopacket.NewSerializeBuffer()
	var id uint16
	for _, b := range src {
		id += uint16(b)
	}
	icmpLayer := layers.ICMPv6{
		TypeCode: layers.CreateICMPv6TypeCode(layers.ICMPv6TypeEchoRequest, 0),
	}
	// The echo Identifier/SeqNumber form the 4-byte ICMPv6 Echo message body. Without this
	// layer the packet is only the 4-byte ICMPv6 base header (type/code/checksum) — shorter
	// than the ICMPv6 echo minimum — which gvisor and real hosts treat as malformed and never
	// answer, so the IPv6 heartbeat/route-registration echo got no reply. Mirrors the Id/Seq
	// that GenICMPPacket sets for IPv4.
	icmpEcho := layers.ICMPv6Echo{
		Identifier: id,
		SeqNumber:  uint16(rand.Intn(math.MaxUint16 + 1)),
	}
	ipLayer := layers.IPv6{
		Version:    6,
		SrcIP:      src,
		DstIP:      dst,
		NextHeader: layers.IPProtocolICMPv6,
		HopLimit:   255,
	}
	// ICMPv6's checksum covers the IPv6 pseudo-header, so (unlike ICMPv4) the network layer
	// must be set for ComputeChecksums to work. Without this the echo request went out with a
	// zero checksum, which receivers reject — so the IPv6 heartbeat never got a reply.
	if err := icmpLayer.SetNetworkLayerForChecksum(&ipLayer); err != nil {
		return nil, fmt.Errorf("failed to set network layer for icmp6 checksum: %w", err)
	}
	opts := gopacket.SerializeOptions{
		FixLengths:       true,
		ComputeChecksums: true,
	}
	err := gopacket.SerializeLayers(buf, opts, &ipLayer, &icmpLayer, &icmpEcho)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize icmp6 packet: %w", err)
	}
	return buf.Bytes(), nil
}

// DetectSupportIPv6 checks whether the Linux kernel has IPv6 enabled by reading /proc/sys/net/ipv6/conf/all/disable_ipv6.
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

// IsValidCIDR returns true if the string is a valid CIDR notation.
func IsValidCIDR(str string) bool {
	_, _, err := net.ParseCIDR(str)
	return err == nil
}
