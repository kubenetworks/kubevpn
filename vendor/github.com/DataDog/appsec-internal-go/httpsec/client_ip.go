package httpsec

import (
	"net"
	"net/textproto"
	"strings"

	"github.com/DataDog/appsec-internal-go/netip"
)

const (
	// RemoteIPTag is the tag name used for the remote HTTP request IP address.
	RemoteIPTag = "network.client.ip"
	// ClientIPTag is the tag name used for the client IP deduced from the HTTP
	// request headers with ClientIP().
	ClientIPTag = "http.client_ip"
)

// ClientIPTags returns the resulting Datadog span tags `http.client_ip`
// containing the client IP and `network.client.ip` containing the remote IP.
// The tags are present only if a valid ip address has been returned by
// ClientIP().
func ClientIPTags(remoteIP, clientIP netip.Addr) (tags map[string]string) {
	remoteIPValid := remoteIP.IsValid()
	clientIPValid := clientIP.IsValid()
	if !remoteIPValid && !clientIPValid {
		return nil
	}

	tags = make(map[string]string, 2)
	if remoteIPValid {
		tags[RemoteIPTag] = remoteIP.String()
	}
	if clientIPValid {
		tags[ClientIPTag] = clientIP.String()
	}
	return tags
}

// ClientIP returns the first public IP address found in the given headers. If
// none is present, it returns the first valid IP address present, possibly
// being a local IP address. The remote address, when valid, is used as fallback
// when no IP address has been found at all.
func ClientIP(hdrs map[string][]string, hasCanonicalHeaders bool, remoteAddr string, monitoredHeaders []string) (remoteIP, clientIP netip.Addr) {
	// Walk IP-related headers
	var foundIP netip.Addr
headersLoop:
	for _, headerName := range monitoredHeaders {
		if hasCanonicalHeaders {
			headerName = textproto.CanonicalMIMEHeaderKey(headerName)
		}

		headerValues, exists := hdrs[headerName]
		if !exists {
			continue // this monitored header is not present
		}

		// Assuming a list of comma-separated IP addresses, split them and build
		// the list of values to try to parse as IP addresses
		var ips []string
		for _, ip := range headerValues {
			ips = append(ips, strings.Split(ip, ",")...)
		}

		// Look for the first valid or global IP address in the comma-separated list
		for _, ipstr := range ips {
			ip := parseIP(strings.TrimSpace(ipstr))
			if !ip.IsValid() {
				continue
			}
			// Replace foundIP if still not valid in order to keep the oldest
			if !foundIP.IsValid() {
				foundIP = ip
			}
			if isGlobal(ip) {
				foundIP = ip
				break headersLoop
			}
		}
	}

	// Decide which IP address is the client one by starting with the remote IP
	if ip := parseIP(remoteAddr); ip.IsValid() {
		remoteIP = ip
		clientIP = ip
	}

	// The IP address found in the headers supersedes a private remote IP address.
	if foundIP.IsValid() && !isGlobal(remoteIP) || isGlobal(foundIP) {
		clientIP = foundIP
	}

	return remoteIP, clientIP
}

func parseIP(s string) netip.Addr {
	if ip, err := netip.ParseAddr(s); err == nil {
		return ip
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		if ip, err := netip.ParseAddr(h); err == nil {
			return ip
		}
	}
	return netip.Addr{}
}

var ipv6SpecialNetworks = [...]netip.Prefix{
	netip.MustParsePrefix("fec0::/10"), // site local
}

func isGlobal(ip netip.Addr) bool {
	// IsPrivate also checks for ipv6 ULA.
	// We care to check for these addresses are not considered public, hence not global.
	// See https://www.rfc-editor.org/rfc/rfc4193.txt for more details.
	isGlobal := ip.IsValid() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()
	if !isGlobal || !ip.Is6() {
		return isGlobal
	}
	for _, n := range ipv6SpecialNetworks {
		if n.Contains(ip) {
			return false
		}
	}
	return isGlobal
}
