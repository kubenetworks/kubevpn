package handler

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	miekgdns "github.com/miekg/dns"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func detectNameserver(ctx context.Context, relovConf *miekgdns.ClientConfig, serviceIP string, podIP string) error {
	domain := config.ConfigMapPodTrafficManager
	err := nameserverChecker(ctx, domain, serviceIP)
	if err != nil {
		relovConf.Servers = []string{podIP}
		plog.G(ctx).Debugf("DNS service use pod IP %s", podIP)
	} else {
		relovConf.Servers = []string{serviceIP}
		plog.G(ctx).Debugf("DNS service use service IP %s", serviceIP)
	}
	return nil
}

func nameserverChecker(ctx context.Context, domain string, dnsServer string) error {
	msg := new(miekgdns.Msg)
	msg.SetQuestion(miekgdns.Fqdn(domain), miekgdns.TypeA)
	client := miekgdns.Client{Net: "udp", Timeout: dnsQueryTimeout}
	_, _, err := client.ExchangeContext(ctx, msg, net.JoinHostPort(dnsServer, strconv.Itoa(config.PortDNS)))
	return err
}

// dnsQueryTimeout bounds a single DNS query against the in-cluster DNS service.
const dnsQueryTimeout = 10 * time.Second

// resolveDomainViaClusterDNS resolves a domain to an IP by querying the
// in-cluster kubevpn DNS forward server directly (over the TUN), replacing the
// previous `dig` shell-out inside the sidecar pod. This is what lets the sidecar
// image drop the dnsutils dependency. The forward server itself performs
// search-list expansion and upstream forwarding, so a plain A/AAAA query is
// enough to resolve cluster-internal names (private-link, ExternalName, etc.).
func resolveDomainViaClusterDNS(ctx context.Context, servers []string, domain string) (net.IP, error) {
	for _, qtype := range []uint16{miekgdns.TypeA, miekgdns.TypeAAAA} {
		for _, server := range servers {
			ip := resolveOnce(ctx, net.JoinHostPort(server, strconv.Itoa(config.PortDNS)), domain, qtype)
			if ip != nil {
				return ip, nil
			}
		}
	}
	return nil, fmt.Errorf("failed to resolve domain %s via cluster DNS", domain)
}

// resolveOnce sends a single DNS query of the given type to addr (host:port)
// and returns the first A/AAAA answer, or nil on error/no record. CNAME chains
// are followed by the resolver, so only the terminal address record is returned.
func resolveOnce(ctx context.Context, addr string, domain string, qtype uint16) net.IP {
	msg := new(miekgdns.Msg)
	msg.SetQuestion(miekgdns.Fqdn(domain), qtype)
	client := miekgdns.Client{Net: "udp", Timeout: dnsQueryTimeout}
	resp, _, err := client.ExchangeContext(ctx, msg, addr)
	if err != nil || resp == nil {
		if err != nil {
			plog.G(ctx).Debugf("Failed to resolve %s via %s: %v", domain, addr, err)
		}
		return nil
	}
	for _, ans := range resp.Answer {
		switch rr := ans.(type) {
		case *miekgdns.A:
			return rr.A
		case *miekgdns.AAAA:
			return rr.AAAA
		}
	}
	return nil
}
