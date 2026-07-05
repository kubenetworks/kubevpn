package util

import (
	"net"
	"net/url"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
)

// CIDRsToString joins CIDRs into a comma-separated string, or "(none)" if empty.
func CIDRsToString(cidrs []*net.IPNet) string {
	if len(cidrs) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(cidrs))
	for _, c := range cidrs {
		if c != nil {
			parts = append(parts, c.String())
		}
	}
	return strings.Join(parts, ", ")
}

// parseCIDRFromFlag extracts CIDRs from Kubernetes component command-line flags
// (--cluster-cidr and --service-cluster-ip-range).
func parseCIDRFromFlag(content string) (result []*net.IPNet) {
	if strings.Contains(content, "cluster-cidr") || strings.Contains(content, "service-cluster-ip-range") {
		_, cidrList, found := strings.Cut(content, "=")
		if found {
			for _, cidr := range strings.Split(cidrList, ",") {
				_, c, _ := net.ParseCIDR(cidr)
				if c != nil {
					result = append(result, c)
				}
			}
		}
	}
	return
}

// RemoveCIDRsContainingIPs removes any CIDR from the slice that contains one of the given IPs.
func RemoveCIDRsContainingIPs(cidrs []*net.IPNet, ipList []net.IP) []*net.IPNet {
	for i := len(cidrs) - 1; i >= 0; i-- {
		for _, ip := range ipList {
			if cidrs[i].Contains(ip) {
				cidrs = append(cidrs[:i], cidrs[i+1:]...)
				break
			}
		}
	}
	return cidrs
}

// RemoveLargerOverlappingCIDRs deduplicates overlapping CIDRs, keeping only the broadest (smallest prefix) in each overlap group.
func RemoveLargerOverlappingCIDRs(cidrNets []*net.IPNet) []*net.IPNet {
	sort.Slice(cidrNets, func(i, j int) bool {
		onesI, _ := cidrNets[i].Mask.Size()
		onesJ, _ := cidrNets[j].Mask.Size()
		// mask number is smaller, means the mask is larger
		return onesI < onesJ
	})

	var cidrsOverlap = func(cidr1, cidr2 *net.IPNet) bool {
		return cidr1.Contains(cidr2.IP) || cidr2.Contains(cidr1.IP)
	}

	var result []*net.IPNet
	skipped := make(map[int]bool)

	for i := range cidrNets {
		if skipped[i] {
			continue
		}
		for j := i + 1; j < len(cidrNets); j++ {
			if cidrsOverlap(cidrNets[i], cidrNets[j]) {
				skipped[j] = true
			}
		}
		result = append(result, cidrNets[i])
	}
	return result
}

// GetAPIServerIP resolves the API server host URL to a list of IP addresses via parsing and DNS lookup.
func GetAPIServerIP(apiServerHost string) ([]net.IP, error) {
	u, err := url.Parse(apiServerHost)
	if err != nil {
		return nil, err
	}

	var host string
	if strings.IndexByte(u.Host, ':') < 0 {
		host = u.Host
	} else {
		host, _, err = net.SplitHostPort(u.Host)
		if err != nil {
			return nil, err
		}
	}

	var ipList []net.IP
	seen := sets.New[string]()
	addIP := func(ip net.IP) {
		key := ip.String()
		if !seen.Has(key) {
			ipList = append(ipList, ip)
			seen.Insert(key)
		}
	}

	if ip := net.ParseIP(host); ip != nil {
		addIP(ip)
	}

	addrs, _ := net.LookupHost(host)
	for _, addr := range addrs {
		if ip := net.ParseIP(addr); ip != nil {
			addIP(ip)
		}
	}
	return ipList, nil
}
