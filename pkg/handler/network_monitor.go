package handler

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"time"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"tailscale.com/net/netmon"
	"tailscale.com/types/logger"
)

// Keep this seam private and limited to the vendored netmon API. The supervisor,
// not netmon or this adapter's lifetime worker, owns every recovery decision.
type networkMonitorSource interface {
	RegisterChangeCallback(netmon.ChangeFunc) func()
	InterfaceState() *netmon.State
	Start()
	Close() error
}

type networkInterface struct {
	index int
	up    bool
	ips   []netip.Prefix
}
type networkSnapshot struct {
	defaultInterface string
	interfaces       map[string]networkInterface
	proxy, pac       string
}
type underlayRoute struct {
	iface        string
	index        int
	gateway, src netip.Addr
	available    bool
}
type networkMonitor struct {
	mu             sync.Mutex // callback gate, pending flags and cached path selection
	source         networkMonitorSource
	closed         bool
	pending        bool
	watched        map[string]bool
	fallback       bool
	ownName        string
	ownIPs         map[netip.Addr]bool
	previousOwnIPs map[netip.Addr]bool
	proxyRelevant  bool
	// Only the supervisor accesses these after publication.
	endpoints       []net.IP
	unknownEndpoint bool
	routes          []underlayRoute
	snapshot        networkSnapshot
}

func copyNetworkSnapshot(s *netmon.State) networkSnapshot {
	out := networkSnapshot{interfaces: make(map[string]networkInterface)}
	if s == nil {
		return out
	}
	out.defaultInterface, out.proxy, out.pac = s.DefaultRouteInterface, s.HTTPProxy, s.PAC
	for name, iface := range s.Interface {
		i := networkInterface{ips: slices.Clone(s.InterfaceIPs[name])}
		if iface.Interface != nil {
			i.index, i.up = iface.Index, iface.Flags&net.FlagUp != 0
		}
		slices.SortFunc(i.ips, func(a, b netip.Prefix) int { return a.Compare(b) })
		i.ips = slices.Compact(i.ips)
		out.interfaces[name] = i
	}
	return out
}
func networkAddr(ip net.IP) netip.Addr {
	a, _ := netip.AddrFromSlice(ip)
	return a.Unmap()
}
func (m *networkMonitor) ownInterface(name string, states ...networkSnapshot) bool {
	if name == m.ownName && name != "" {
		return true
	}
	for _, s := range states {
		for _, p := range s.interfaces[name].ips {
			if m.ownIPs[p.Addr().Unmap()] || m.previousOwnIPs[p.Addr().Unmap()] {
				return true
			}
		}
	}
	return false
}

// Called under m.mu. Only selected underlay interfaces matter; a corporate VPN
// is an ordinary underlay. Never infer ownership from tun/utun name prefixes or
// prefix containment (a peer's /16 is not ownership of every address in it).
func (m *networkMonitor) relevant(old, next networkSnapshot) bool {
	names := make(map[string]bool, len(m.watched)+2)
	for name := range m.watched {
		names[name] = true
	}
	if m.fallback {
		names[old.defaultInterface], names[next.defaultInterface] = true, true
		if old.defaultInterface != next.defaultInterface && !m.ownInterface(old.defaultInterface, old, next) && !m.ownInterface(next.defaultInterface, old, next) {
			return true
		}
	}
	for name := range names {
		if name == "" || m.ownInterface(name, old, next) {
			continue
		}
		a, aok := old.interfaces[name]
		b, bok := next.interfaces[name]
		if aok != bok || a.index != b.index || a.up != b.up || !slices.Equal(a.ips, b.ips) {
			return true
		}
	}
	// Native PAC/HTTPProxy state is not used by the ordinary Go direct/SSH
	// dialers. Only a configured REST proxy policy may consume it.
	return m.proxyRelevant && (old.proxy != next.proxy || old.pac != next.pac)
}
func (nm *NetworkManager) startNetworkMonitor() {
	// Capture TUN identity after creation, before the worker is published. Stop
	// waits for this worker before clearing the device name.
	m := &networkMonitor{ownName: nm.tunName, ownIPs: make(map[netip.Addr]bool), watched: make(map[string]bool), fallback: true, proxyRelevant: nm.cfg.Config != nil && nm.cfg.Config.Proxy != nil}
	nm.startWorker(func() {
		factory := nm.newNetworkMonitor
		if factory == nil {
			factory = func(logf logger.Logf) (networkMonitorSource, error) { return netmon.New(logf) }
		}
		source, err := factory(func(format string, args ...any) { plog.G(nm.ctx).Debugf(format, args...) })
		if err != nil {
			plog.G(nm.ctx).Warnf("Network monitor unavailable; continuing heartbeat watchdog: %v", err)
			return
		}
		m.source = source
		defer func() {
			if err := source.Close(); err != nil {
				plog.G(nm.ctx).Debugf("Close network monitor: %v", err)
			}
		}()
		m.endpoints, m.unknownEndpoint = nm.networkEndpoints(nm.ctx)
		m.snapshot = copyNetworkSnapshot(source.InterfaceState())
		nm.sampleNetworkRoutes(m)
		unregister := source.RegisterChangeCallback(func(delta *netmon.ChangeDelta) {
			// Callbacks can arrive arbitrarily late and out of order. Only time
			// jumps are sticky; ordinary callbacks wake the supervisor to compare
			// current source state/routes, never historical Old/New snapshots.
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.closed || nm.ctx.Err() != nil || delta == nil {
				return
			}
			m.pending = m.pending || delta.TimeJumped
			select {
			case nm.monitorChanges <- struct{}{}:
			default:
			}
		})
		defer func() { m.mu.Lock(); m.closed = true; m.mu.Unlock(); unregister() }()
		nm.transportMu.Lock()
		if nm.transportStopped || nm.ctx.Err() != nil {
			nm.transportMu.Unlock()
			return
		}
		nm.networkMonitor = m
		nm.transportMu.Unlock()
		source.Start()
		<-nm.ctx.Done()
		// This is lifetime ownership only, not a second recovery/cancel loop.
	})
}

// Resolve configured SSH Addr once with a cancellation/deadline bound. Config
// aliases are not Host(): absent/unknown aliases fall back to the default route.
// Forwarded loopback API endpoints never masquerade as a WAN path.
func (nm *NetworkManager) networkEndpoints(ctx context.Context) ([]net.IP, bool) {
	var ips []net.IP
	add := func(ip net.IP) {
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
			return
		}
		for _, known := range ips {
			if known.Equal(ip) {
				return
			}
		}
		ips = append(ips, slices.Clone(ip))
	}
	resolve := func(host string) bool {
		lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		resolved, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
		usable := false
		for _, ip := range resolved {
			if !ip.IP.IsLoopback() && !ip.IP.IsUnspecified() {
				add(ip.IP)
				usable = true
			}
		}
		return err == nil && usable
	}
	unknown, directAPI := false, true
	if cfg := nm.cfg.Config; cfg != nil {
		// Match client-go's SPDY/HTTP transport policy for the API request.
		// Never dial the proxy, change the configuration, or log its URL/error
		// (either may contain credentials). Resolve its egress once at startup.
		proxy := http.ProxyFromEnvironment
		if cfg.Proxy != nil {
			proxy = cfg.Proxy
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Host, nil)
		if err != nil {
			unknown, directAPI = true, false
		} else if selected, err := proxy(req); err != nil {
			unknown, directAPI = true, false
		} else if selected != nil {
			directAPI = false
			unknown = !resolve(selected.Hostname())
		}
	}
	if directAPI {
		for _, ip := range nm.cfg.APIServerIPs {
			add(ip)
		}
	}
	if conf := nm.cfg.SshConf; conf != nil && !conf.IsEmpty() {
		if conf.Addr == "" {
			unknown = true
		} else {
			host, _, err := net.SplitHostPort(conf.Addr)
			if err != nil {
				host = conf.Addr
			}
			if !resolve(host) {
				unknown = true
			}
		}
	}
	return ips, unknown
}

// One fresh routing snapshot per supervisor tick/event for known endpoints and,
// when needed, two representative default destinations. No DNS polling,
// per-packet route lookup, outbound probes, or route mutation here.
func (nm *NetworkManager) sampleNetworkRoutes(m *networkMonitor) (changed bool) {
	var routes []underlayRoute
	watched := make(map[string]bool)
	fallback := m.unknownEndpoint || len(m.endpoints) == 0
	resolver := nm.routes()
	lookup := func(ip net.IP) underlayRoute {
		var route underlayRoute
		if resolver != nil {
			iface, gw, src, err := resolver.Route(ip)
			if err == nil && iface != nil && iface.Flags&net.FlagLoopback == 0 {
				route = underlayRoute{iface.Name, iface.Index, networkAddr(gw), networkAddr(src), iface.Flags&net.FlagUp != 0}
			}
		}
		return route
	}
	for _, ip := range m.endpoints {
		route := lookup(ip)
		routes = append(routes, route)
		if route.available {
			watched[route.iface] = true
		} else {
			fallback = true
		}
	}
	if fallback {
		// Read-only representative route lookups, not packets or reachability
		// probes. Both families matter when an alias/forwarded endpoint hides
		// the actual WAN destination. Same-interface gateway/src changes can
		// otherwise be suppressed by netmon's equal-state optimization.
		for _, ip := range []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("2606:4700:4700::1111")} {
			route := lookup(ip)
			routes = append(routes, route)
			if route.available {
				watched[route.iface] = true
			}
		}
	}
	var reserved []net.IP
	if nm.cfg.ReservedTunIPs != nil {
		reserved = nm.cfg.ReservedTunIPs()
	}
	ownIPs := make(map[netip.Addr]bool, len(reserved)+2)
	for _, ip := range reserved {
		ownIPs[networkAddr(ip)] = true
	}
	// ChangeTunIP runs in the watcher, so do not read its mutable IP pointers
	// from this supervisor. netmon publishes immutable address snapshots; the
	// device's captured name remains its primary ownership identity.
	if state := m.source.InterfaceState(); state != nil {
		for _, prefix := range state.InterfaceIPs[m.ownName] {
			ownIPs[prefix.Addr().Unmap()] = true
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.previousOwnIPs, m.ownIPs = m.ownIPs, ownIPs
	changed = m.routes != nil && !slices.Equal(m.routes, routes)
	m.routes, m.watched, m.fallback = routes, watched, fallback
	return changed
}

// Missing default connectivity slows retries, but never disables the watchdog:
// a route snapshot may be incomplete and recovery must remain bounded.
func (nm *NetworkManager) networkOffline() bool {
	nm.transportMu.Lock()
	m := nm.networkMonitor
	nm.transportMu.Unlock()
	if m == nil {
		return false
	}
	for _, route := range m.routes {
		if route.available {
			return false
		}
	}
	name := m.snapshot.defaultInterface
	iface, ok := m.snapshot.interfaces[name]
	return name == "" || !ok || !iface.up
}

func (nm *NetworkManager) waitNetworkRetry(ctx context.Context, delay time.Duration) bool {
	nm.consumeNetworkChange()
	if nm.networkOffline() && delay < livenessCheckInterval {
		delay = livenessCheckInterval
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		case <-nm.monitorChanges:
			if nm.consumeNetworkChange() && !nm.networkOffline() {
				return true
			}
		}
	}
}

// Only the supervisor calls this. Read the source's current snapshot rather than
// a callback's New: netmon's goroutine scheduling does not preserve event order.
func (nm *NetworkManager) consumeNetworkChange() bool {
	nm.transportMu.Lock()
	m := nm.networkMonitor
	nm.transportMu.Unlock()
	if m == nil || nm.ctx.Err() != nil {
		return false
	}
	routeChanged := nm.sampleNetworkRoutes(m)
	next := copyNetworkSnapshot(m.source.InterfaceState())
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := m.pending || routeChanged || m.relevant(m.snapshot, next)
	m.pending = false
	m.snapshot = next
	return changed
}
