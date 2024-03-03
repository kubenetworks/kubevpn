package handler

import (
	"net"
	"sort"
)

type Connects []*ConnectOptions

func (s Connects) Len() int {
	return len(s)
}

func (s Connects) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s Connects) Append(options *ConnectOptions) Connects {
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
	for _, extraCIDR := range b.ExtraRouteInfo.ExtraCIDR {
		ip, cidr, err := net.ParseCIDR(extraCIDR)
		if err != nil {
			continue
		}
		if containsFunc(cidr, a.apiServerIPs) {
			return true
		}
		for _, p := range a.apiServerIPs {
			if ip.Equal(p) {
				return true
			}
		}
	}
	for _, entry := range b.extraHost {
		ip := net.ParseIP(entry.IP)
		if ip == nil || ip.IsLoopback() {
			continue
		}
		for _, p := range a.apiServerIPs {
			if ip.Equal(p) {
				return true
			}
		}
	}
	return false
}

// Sort ...
// base order: first connect last disconnect
// sort by dependency
func (s Connects) Sort() Connects {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
	sort.Stable(s)
	return s
}
