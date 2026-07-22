package handler

import (
	"net"
	"slices"
	"sort"

	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
)

// Connects is a sortable slice of Connection ordered by cross-cluster dependency.
type Connects []Connection

func (s Connects) Len() int {
	return len(s)
}

func (s Connects) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

// Append adds a non-nil Connection to the slice and returns the result.
func (s Connects) Append(options Connection) Connects {
	if options != nil {
		return append(s, options)
	}
	return s
}

// Less ...
/**
assume: clusterA and clusterB in same VPC network, but only clusterA api-server have public ip,
we need access clusterB via clusterA network.
steps:
  - connect to clusterA with options --extra-cidr or --extra-domain (which container clusterB api-server address)
  - connect to clusterB
when we disconnect from all:
first, we need to disconnect clusterB
second, disconnect clusterA
*/
func (s Connects) Less(i, j int) bool {
	a := s[i]
	b := s[j]

	if a == nil {
		return true
	}

	var containsFunc = func(cidr *net.IPNet, ips []net.IP) bool {
		for _, ip := range ips {
			if !ip.IsLoopback() && cidr.Contains(ip) {
				return true
			}
		}
		return false
	}
	// API server IPs and extra hosts are owned by the data plane (DataSession); the
	// control-plane ConnectOptions does not satisfy DataPlane, so the assertion fails
	// there and both slices stay nil — exactly the old stub behavior (no dependency
	// detected among user-daemon connections, which is correct: dependency ordering
	// only matters at the data plane where TUN IPs/routes live).
	var aAPIServerIPs []net.IP
	var bExtraHosts []dns.Entry
	if dp, ok := a.(DataPlane); ok {
		aAPIServerIPs = dp.GetAPIServerIPs()
	}
	if dp, ok := b.(DataPlane); ok {
		bExtraHosts = dp.GetNetworkExtraHost()
	}
	for _, extraCIDR := range b.GetExtraCIDR() {
		ip, cidr, err := net.ParseCIDR(extraCIDR)
		if err != nil {
			continue
		}
		if containsFunc(cidr, aAPIServerIPs) {
			return true
		}
		if slices.ContainsFunc(aAPIServerIPs, ip.Equal) {
			return true
		}
	}
	for _, entry := range bExtraHosts {
		ip := net.ParseIP(entry.IP)
		if ip == nil || ip.IsLoopback() {
			continue
		}
		if slices.ContainsFunc(aAPIServerIPs, ip.Equal) {
			return true
		}
	}
	return false
}

// Sort orders connections so that dependent clusters disconnect before their dependencies.
func (s Connects) Sort() Connects {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
	sort.Stable(s)
	return s
}
