package etcd

import (
	"context"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/request"

	"github.com/miekg/dns"
)

// ServeDNS implements the plugin.Handler interface.
func (e *Etcd) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	opt := plugin.Options{}
	state := request.Request{W: w, Req: r}

	zone := plugin.Zones(e.Zones).Matches(state.Name())
	if zone == "" {
		return plugin.NextOrFailure(e.Name(), e.Next, ctx, w, r)
	}

	var (
		records, extra []dns.RR
		truncated      bool
		err            error
	)

	switch state.QType() {
	case dns.TypeA:
		records, truncated, err = plugin.A(ctx, e, zone, state, nil, opt)
	case dns.TypeAAAA:
		records, truncated, err = plugin.AAAA(ctx, e, zone, state, nil, opt)
	case dns.TypeTXT:
		records, truncated, err = plugin.TXT(ctx, e, zone, state, nil, opt)
	case dns.TypeCNAME:
		records, err = plugin.CNAME(ctx, e, zone, state, opt)
	case dns.TypePTR:
		records, err = plugin.PTR(ctx, e, zone, state, opt)
	case dns.TypeMX:
		records, extra, err = plugin.MX(ctx, e, zone, state, opt)
	case dns.TypeSRV:
		records, extra, err = plugin.SRV(ctx, e, zone, state, opt)
	case dns.TypeSOA:
		records, err = plugin.SOA(ctx, e, zone, state, opt)
	case dns.TypeNS:
		if state.Name() == zone {
			records, extra, err = plugin.NS(ctx, e, zone, state, opt)
			break
		}
		fallthrough
	default:
		// Do a fake A lookup, so we can distinguish between NODATA and NXDOMAIN
		_, _, err = plugin.A(ctx, e, zone, state, nil, opt)
	}
	if err != nil && e.IsNameError(err) {
		if e.Fall.Through(state.Name()) {
			return plugin.NextOrFailure(e.Name(), e.Next, ctx, w, r)
		}
		// Make err nil when returning here, so we don't log spam for NXDOMAIN.
		return plugin.BackendError(ctx, e, zone, dns.RcodeNameError, state, nil /* err */, opt)
	}
	if err != nil {
		return plugin.BackendError(ctx, e, zone, dns.RcodeServerFailure, state, err, opt)
	}

	if len(records) == 0 {
		return plugin.BackendError(ctx, e, zone, dns.RcodeSuccess, state, err, opt)
	}

	m := new(dns.Msg)
	m.SetReply(r)
	m.Truncated = truncated
	m.Authoritative = true
	m.Answer = records
	m.Extra = extra

	w.WriteMsg(m)
	return dns.RcodeSuccess, nil
}

// Name implements the Handler interface.
func (e *Etcd) Name() string { return "etcd" }
