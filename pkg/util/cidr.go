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

const (
	// cidrMergeFloorV4 is the broadest (smallest-prefix) IPv4 supernet mergeToSupernet
	// is allowed to produce. /12 covers every common Service/Pod CIDR default (/12, /16,
	// /20, /24) and reconstructs the standard 10.96.0.0/12 from scattered /24s, while
	// refusing to merge far-apart IPs into an over-broad route that could hijack local networks.
	cidrMergeFloorV4 = 12
	// cidrMergeFloorV6 is the IPv6 counterpart: /64 inference is already broad, so only
	// members sharing a /48 prefix are merged.
	cidrMergeFloorV6 = 48
)

// mergeToSupernet groups CIDRs by IP family and, per family, replaces the members with the
// single smallest supernet that covers them all — but only when that supernet's prefix length
// is >= the family floor (cidrMergeFloorV4 / cidrMergeFloorV6). A cluster generally has one
// Service CIDR and one Pod CIDR range, so /24 (or /64) ranges inferred from individual
// ClusterIPs/PodIPs are coalesced into the enclosing range. When the members are too far apart
// to plausibly belong to one range (common prefix shorter than the floor), they are returned
// unchanged (deduplicated) so a pathological spread never yields an over-broad route. A family
// with a single member is returned as-is.
func mergeToSupernet(cidrs []*net.IPNet) []*net.IPNet {
	var v4, v6 []*net.IPNet
	for _, c := range cidrs {
		if c == nil {
			continue
		}
		if c.IP.To4() != nil {
			v4 = append(v4, c)
		} else {
			v6 = append(v6, c)
		}
	}
	var result []*net.IPNet
	result = append(result, mergeFamily(v4, 32, cidrMergeFloorV4)...)
	result = append(result, mergeFamily(v6, 128, cidrMergeFloorV6)...)
	return result
}

// mergeFamily merges a single IP family's CIDRs (all bits-wide: 32 for IPv4, 128 for IPv6).
// It returns one supernet when the members' longest common prefix is >= floor, otherwise the
// deduplicated members unchanged.
func mergeFamily(cidrs []*net.IPNet, bits, floor int) []*net.IPNet {
	// Deduplicate members by string form; also find the narrowest member prefix so the
	// merged supernet is never narrower than an input.
	seen := sets.New[string]()
	var members []*net.IPNet
	narrowest := bits
	for _, c := range cidrs {
		if seen.Has(c.String()) {
			continue
		}
		seen.Insert(c.String())
		members = append(members, c)
		if ones, _ := c.Mask.Size(); ones < narrowest {
			narrowest = ones
		}
	}
	if len(members) <= 1 {
		return members
	}

	// Longest common prefix (in bits) across all member network addresses.
	common := narrowest
	base := canonicalIP(members[0].IP, bits)
	for _, m := range members[1:] {
		if l := commonPrefixLen(base, canonicalIP(m.IP, bits)); l < common {
			common = l
		}
	}
	if common < floor {
		return members
	}

	mask := net.CIDRMask(common, bits)
	supernet := &net.IPNet{IP: base.Mask(mask), Mask: mask}
	return []*net.IPNet{supernet}
}

// canonicalIP normalizes an IP to its family-native byte length (4 for IPv4, 16 for IPv6).
func canonicalIP(ip net.IP, bits int) net.IP {
	if bits == 32 {
		return ip.To4()
	}
	return ip.To16()
}

// commonPrefixLen returns the number of leading bits shared by two equal-length IPs.
func commonPrefixLen(a, b net.IP) int {
	n := 0
	for i := 0; i < len(a) && i < len(b); i++ {
		x := a[i] ^ b[i]
		if x == 0 {
			n += 8
			continue
		}
		for bit := 7; bit >= 0; bit-- {
			if x&(1<<uint(bit)) == 0 {
				n++
			} else {
				return n
			}
		}
	}
	return n
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
