package network

import (
	"context"
	"errors"
	"github.com/wencaiwulue/kubevpn/v2/pkg/internal/wire"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tunnel"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"tailscale.com/net/netmon"
	"tailscale.com/types/logger"
)

// The source controls OS notifications only; forwarding and supervisor lifecycles
// use the real SPDY fixture. No host interfaces or routes are changed.
type testNetworkMonitor struct {
	mu        sync.Mutex
	state     *netmon.State
	callback  netmon.ChangeFunc
	started   chan struct{}
	closed    chan struct{}
	closeGate <-chan struct{}
}

func (m *testNetworkMonitor) InterfaceState() *netmon.State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}
func (m *testNetworkMonitor) RegisterChangeCallback(cb netmon.ChangeFunc) func() {
	m.mu.Lock()
	m.callback = cb
	m.mu.Unlock()
	return func() { m.mu.Lock(); m.callback = nil; m.mu.Unlock() }
}
func (m *testNetworkMonitor) Start() { close(m.started) }
func (m *testNetworkMonitor) Close() error {
	if m.closeGate != nil {
		<-m.closeGate
	}
	close(m.closed)
	return nil
}
func (m *testNetworkMonitor) emit(next *netmon.State, jump bool) {
	m.mu.Lock()
	old, cb := m.state, m.callback
	m.state = next
	m.mu.Unlock()
	if cb != nil {
		cb(&netmon.ChangeDelta{Old: old, New: next, TimeJumped: jump, Major: true})
	}
}
func monitorState(def string) *netmon.State {
	s := &netmon.State{DefaultRouteInterface: def, Interface: map[string]netmon.Interface{}, InterfaceIPs: map[string][]netip.Prefix{}}
	for i, name := range []string{"en0", "en1", "utun-corporate", "utun-own", "utun-peer", "unrelated"} {
		s.Interface[name] = netmon.Interface{Interface: &net.Interface{Name: name, Index: i + 1, Flags: net.FlagUp}}
		s.InterfaceIPs[name] = []netip.Prefix{netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 0, 2, byte(i + 1)}), 24)}
	}
	s.InterfaceIPs["utun-own"] = []netip.Prefix{netip.MustParsePrefix("198.18.0.10/16")}
	s.InterfaceIPs["utun-peer"] = []netip.Prefix{netip.MustParsePrefix("::ffff:198.18.0.11/128")}
	return s
}

type monitorRoutes struct {
	mu          sync.Mutex
	iface       string
	gateway     string
	src         string
	unavailable bool
	calls       atomic.Int32
}

func (r *monitorRoutes) Route(net.IP) (*net.Interface, net.IP, net.IP, error) {
	r.calls.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.unavailable {
		return nil, nil, nil, errors.New("no route")
	}
	return &net.Interface{Name: r.iface, Index: 3, Flags: net.FlagUp}, net.ParseIP(r.gateway), net.ParseIP(r.src), nil
}
func monitorFixture(t *testing.T, state *netmon.State, routes *monitorRoutes) (*Runtime, *testNetworkMonitor, *atomic.Int32) {
	t.Helper()
	srv, count := transportForwardServer(t, nil)
	nm := transportManager(t, srv.URL)
	nm.device.tunName = "utun-own"
	nm.addresses.localTunIPv4 = &net.IPNet{IP: net.ParseIP("198.18.0.10"), Mask: net.CIDRMask(32, 32)}
	nm.cfg.ReservedTunIPs = func() []net.IP { return []net.IP{net.ParseIP("198.18.0.11")} }
	nm.routeTable.newRouteResolver = func() routeResolver { return endpointMonitorRoutes{} }
	if routes != nil {
		nm.cfg.APIServerIPs = []net.IP{net.ParseIP("203.0.113.1")}
		nm.routeTable.newRouteResolver = func() routeResolver { return routes }
	}
	m := &testNetworkMonitor{state: state, started: make(chan struct{}), closed: make(chan struct{})}
	nm.transport.newMonitor = func(logger.Logf) (networkMonitorSource, error) { return m, nil }
	nm.transport.startNetworkMonitor(nm.device.tunName)
	transportAwait(t, m.started)
	nm.transport.launchTransportSupervisor()
	ctx, cancel := context.WithTimeout(nm.transport.ctx, 3*time.Second)
	defer cancel()
	if _, err := nm.transport.waitTransport(ctx); err != nil {
		t.Fatal(err)
	}
	return nm, m, count
}
func monitorWait(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !f() {
		select {
		case <-deadline.C:
			t.Fatal("monitor condition timed out")
		case <-tick.C:
		}
	}
}
func monitorQuiet(t *testing.T, nm *Runtime, last time.Time) {
	t.Helper()
	select {
	case <-time.After(80 * time.Millisecond):
	case <-nm.transport.ctx.Done():
		t.Fatal("manager canceled")
	}
	if !nm.transport.heartbeatStats.LastReply().Equal(last) {
		t.Fatal("cosmetic event reset health")
	}
}

// These use the running PF supervisor, but hold notifications so only its
// existing tick can discover a routing/state change.
func monitorTickWait(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.NewTimer(livenessCheckInterval + 2*time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !f() {
		select {
		case <-deadline.C:
			t.Fatal("supervisor tick missed network change")
		case <-tick.C:
		}
	}
}

type endpointMonitorRoutes struct {
	api, proxy, v4, v6 *monitorRoutes
}

func (r endpointMonitorRoutes) Route(ip net.IP) (*net.Interface, net.IP, net.IP, error) {
	if ip.Equal(net.ParseIP("203.0.113.1")) && r.api != nil {
		return r.api.Route(ip)
	}
	if ip.Equal(net.ParseIP("203.0.113.9")) && r.proxy != nil {
		return r.proxy.Route(ip)
	}
	if ip.To4() == nil && r.v6 != nil {
		return r.v6.Route(ip)
	}
	if ip.To4() != nil && r.v4 != nil {
		return r.v4.Route(ip)
	}
	return nil, nil, nil, errors.New("no route")
}

func TestNetworkMonitorReview_DefaultFingerprintWithoutKnownEndpoint(t *testing.T) {
	for _, v6 := range []bool{false, true} {
		t.Run(map[bool]string{false: "ipv4", true: "ipv6"}[v6], func(t *testing.T) {
			srv, count := transportForwardServer(t, nil)
			nm := transportManager(t, srv.URL)
			// Start PF before adding an alias solely to the monitor's endpoint view.
			nm.transport.launchTransportSupervisor()
			ctx, cancel := context.WithTimeout(nm.transport.ctx, time.Second)
			defer cancel()
			if _, err := nm.transport.waitTransport(ctx); err != nil {
				t.Fatal(err)
			}
			nm.cfg.SshConf = &ssh.SshConfig{ConfigAlias: "unknown-alias"}
			nm.cfg.APIServerIPs = []net.IP{net.ParseIP("127.0.0.1")}
			r := &monitorRoutes{iface: "en0", gateway: "192.0.2.1", src: "192.0.2.2"}
			resolver := endpointMonitorRoutes{v4: r}
			if v6 {
				r.gateway = "2001:db8::1"
				r.src = "2001:db8::2"
				resolver = endpointMonitorRoutes{v6: r}
			}
			nm.routeTable.newRouteResolver = func() routeResolver { return resolver }
			m := &testNetworkMonitor{state: monitorState("en0"), started: make(chan struct{}), closed: make(chan struct{})}
			nm.transport.newMonitor = func(logger.Logf) (networkMonitorSource, error) { return m, nil }
			nm.transport.startNetworkMonitor(nm.device.tunName)
			transportAwait(t, m.started)
			nm.transport.heartbeatStats.MarkReply()
			r.mu.Lock()
			if v6 {
				r.gateway = "2001:db8::ffff"
				r.src = "2001:db8::3"
			} else {
				r.gateway = "192.0.2.254"
				r.src = "192.0.2.3"
			}
			r.mu.Unlock()
			monitorTickWait(t, func() bool { return nm.LastHeartbeat().IsZero() })
			if r.calls.Load() < 2 || count.Load() != 1 {
				t.Fatal("fallback did not compare routes without reconnecting")
			}
			nm.Stop()
		})
	}
}

func TestNetworkMonitorReview_EnvironmentProxyEgress(t *testing.T) {
	// net/http caches proxy environment once per process. A subprocess verifies
	// the real default policy without mutating/resetting transport global state.
	if os.Getenv("KUBEVPN_MONITOR_PROXY_TEST") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestNetworkMonitorReview_EnvironmentProxyEgress$", "-test.count=1")
		for _, e := range os.Environ() {
			key, _, _ := strings.Cut(e, "=")
			switch strings.ToUpper(key) {
			case "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "ALL_PROXY", "REQUEST_METHOD":
				continue
			}
			cmd.Env = append(cmd.Env, e)
		}
		cmd.Env = append(cmd.Env, "KUBEVPN_MONITOR_PROXY_TEST=1", "HTTPS_PROXY=http://user:secret@203.0.113.9:3128")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("environment proxy regression: %v\n%s", err, output)
		}
		return
	}
	testMonitorProxyEgress(t, true)
}

func TestNetworkMonitorReview_ConfiguredProxyEgress(t *testing.T) {
	testMonitorProxyEgress(t, false)
}

func testMonitorProxyEgress(t *testing.T, environment bool) {
	srv, count := transportForwardServer(t, nil)
	nm := transportManager(t, srv.URL)
	nm.transport.launchTransportSupervisor()
	ctx, cancel := context.WithTimeout(nm.transport.ctx, time.Second)
	defer cancel()
	if _, err := nm.transport.waitTransport(ctx); err != nil {
		t.Fatal(err)
	}
	// Monitor the proxy selected for this API URL, not the API destination's
	// stable direct route. The proxy is never dialed by this fixture.
	nm.cfg.Config.Host = "https://api.example.invalid"
	nm.cfg.APIServerIPs = []net.IP{net.ParseIP("203.0.113.1")}
	var selections atomic.Int32
	nm.cfg.Config.Proxy = func(req *http.Request) (*url.URL, error) {
		selections.Add(1)
		if req.URL.Host != "api.example.invalid" {
			return nil, errors.New("wrong API request")
		}
		return url.Parse("http://user:secret@203.0.113.9:3128")
	}
	if environment {
		nm.cfg.Config.Proxy = nil
	}
	api := &monitorRoutes{iface: "en0", gateway: "192.0.2.1", src: "192.0.2.2"}
	proxy := &monitorRoutes{iface: "utun-corporate", gateway: "192.0.2.3", src: "192.0.2.4"}
	nm.routeTable.newRouteResolver = func() routeResolver { return endpointMonitorRoutes{api: api, proxy: proxy} }
	m := &testNetworkMonitor{state: monitorState("en0"), started: make(chan struct{}), closed: make(chan struct{})}
	nm.transport.newMonitor = func(logger.Logf) (networkMonitorSource, error) { return m, nil }
	nm.transport.startNetworkMonitor(nm.device.tunName)
	transportAwait(t, m.started)
	nm.transport.heartbeatStats.MarkReply()
	proxy.mu.Lock()
	proxy.gateway = "192.0.2.254"
	proxy.mu.Unlock()
	monitorTickWait(t, func() bool { return nm.LastHeartbeat().IsZero() })
	wantSelections := int32(1)
	if environment {
		wantSelections = 0
	}
	if selections.Load() != wantSelections || proxy.calls.Load() < 2 || api.calls.Load() != 0 || count.Load() != 1 {
		t.Fatal("proxy policy/path not selected once in place of direct API route")
	}
	nm.Stop()
}

func TestNetworkMonitorReview_UnknownProxyFallsBack(t *testing.T) {
	srv, count := transportForwardServer(t, nil)
	nm := transportManager(t, srv.URL)
	nm.transport.launchTransportSupervisor()
	ctx, cancel := context.WithTimeout(nm.transport.ctx, time.Second)
	defer cancel()
	if _, err := nm.transport.waitTransport(ctx); err != nil {
		t.Fatal(err)
	}
	nm.cfg.APIServerIPs = []net.IP{net.ParseIP("203.0.113.1")}
	nm.cfg.Config.Proxy = func(*http.Request) (*url.URL, error) { return nil, errors.New("proxy selection unavailable") }
	r := &monitorRoutes{iface: "en0", gateway: "192.0.2.1", src: "192.0.2.2"}
	api := &monitorRoutes{iface: "utun-corporate", gateway: "192.0.2.3", src: "192.0.2.4"}
	nm.routeTable.newRouteResolver = func() routeResolver { return endpointMonitorRoutes{api: api, v4: r} }
	m := &testNetworkMonitor{state: monitorState("en0"), started: make(chan struct{}), closed: make(chan struct{})}
	nm.transport.newMonitor = func(logger.Logf) (networkMonitorSource, error) { return m, nil }
	nm.transport.startNetworkMonitor(nm.device.tunName)
	transportAwait(t, m.started)
	if !nm.transport.monitor.unknownEndpoint || r.calls.Load() == 0 || api.calls.Load() != 0 {
		t.Fatal("unknown proxy did not select default fallback")
	}
	nm.transport.heartbeatStats.MarkReply()
	r.mu.Lock()
	r.src = "192.0.2.8"
	r.mu.Unlock()
	m.emit(monitorState("en0"), false)
	monitorWait(t, func() bool { return nm.LastHeartbeat().IsZero() })
	if count.Load() != 1 {
		t.Fatal("unknown proxy rotated healthy transport")
	}
	nm.Stop()
}

func TestNetworkMonitorReview_DelayedConsumedDeltaKeepsFreshEcho(t *testing.T) {
	nm, m, count := monitorHeartbeatClient(t)
	m.mu.Lock()
	old, callback := m.state, m.callback
	next := monitorState("en1")
	m.state = next
	m.mu.Unlock()
	before := nm.transport.heartbeatStats.Snapshot()
	monitorTickWait(t, func() bool {
		nm.transport.monitor.mu.Lock()
		seen := nm.transport.monitor.snapshot.defaultInterface == "en1"
		nm.transport.monitor.mu.Unlock()
		return seen && nm.transport.heartbeatStats.Snapshot().Replies > before.Replies
	})
	// Wait for a valid post-reset reply, then deliver the old notification.
	monitorWait(t, func() bool { return !nm.LastHeartbeat().IsZero() })
	fresh := nm.transport.heartbeatStats.Snapshot()
	callback(&netmon.ChangeDelta{Old: old, New: next})
	select {
	case <-time.After(120 * time.Millisecond):
	case <-nm.transport.ctx.Done():
		t.Fatal("manager canceled")
	}
	after := nm.transport.heartbeatStats.Snapshot()
	if after.Replies != fresh.Replies || after.LastReply != fresh.LastReply || after.Generation != fresh.Generation || count.Load() != 1 {
		t.Fatal("delayed already-consumed transition requested fresh evidence again")
	}
	// A transient excursion reverted before inspection must not invalidate
	// currently proven health. Historical delta contents are only wake hints.
	callback(&netmon.ChangeDelta{Old: next, New: monitorState("en0")})
	monitorQuiet(t, nm, after.LastReply)
	if nm.transport.heartbeatStats.Snapshot().Replies != after.Replies {
		t.Fatal("reverted historical change requested another probe")
	}
	fresh = nm.transport.heartbeatStats.Snapshot()
	callback(&netmon.ChangeDelta{Old: next, New: next, TimeJumped: true})
	monitorWait(t, func() bool { return nm.transport.heartbeatStats.Snapshot().Replies > fresh.Replies })
}

func TestNetworkMonitorReview_TwoTransitionsThenLateFirstCallback(t *testing.T) {
	nm, m, count := monitorHeartbeatClient(t)
	m.mu.Lock()
	a, callback := m.state, m.callback
	m.mu.Unlock()
	b, c := monitorState("en1"), monitorState("utun-corporate")
	for _, next := range []*netmon.State{b, c} {
		before := nm.transport.heartbeatStats.Snapshot()
		m.emit(next, false)
		monitorWait(t, func() bool {
			nm.transport.monitor.mu.Lock()
			seen := nm.transport.monitor.snapshot.defaultInterface == next.DefaultRouteInterface
			nm.transport.monitor.mu.Unlock()
			s := nm.transport.heartbeatStats.Snapshot()
			return seen && s.Replies > before.Replies && !s.LastReply.IsZero()
		})
	}
	fresh := nm.transport.heartbeatStats.Snapshot()
	callback(&netmon.ChangeDelta{Old: a, New: b})
	monitorQuiet(t, nm, fresh.LastReply)
	after := nm.transport.heartbeatStats.Snapshot()
	if after.Replies != fresh.Replies || after.Generation != fresh.Generation || count.Load() != 1 {
		t.Fatal("late A->B callback invalidated fresh health after B->C")
	}
}

func TestNetworkMonitorReview_ReservedAddressesExpire(t *testing.T) {
	r := &monitorRoutes{iface: "utun-corporate", gateway: "192.0.2.1", src: "192.0.2.3"}
	nm, m, count := monitorFixture(t, monitorState("en0"), r)
	var reserved atomic.Pointer[net.IP]
	ip := net.ParseIP("192.0.2.3")
	reserved.Store(&ip)
	nm.cfg.ReservedTunIPs = func() []net.IP {
		if ip := reserved.Load(); ip != nil {
			return []net.IP{*ip}
		}
		return nil
	}
	// Seed one reserved snapshot, then age it out over two consumed snapshots.
	for i := range 3 {
		if i == 1 {
			reserved.Store(nil)
		}
		nm.transport.heartbeatStats.MarkReply()
		m.emit(monitorState("en0"), true)
		monitorWait(t, func() bool { return nm.LastHeartbeat().IsZero() })
	}
	nm.transport.heartbeatStats.MarkReply()
	next := monitorState("en0")
	next.InterfaceIPs["utun-corporate"] = []netip.Prefix{netip.MustParsePrefix("192.0.2.30/24")}
	m.emit(next, false)
	monitorWait(t, func() bool { return nm.LastHeartbeat().IsZero() })
	if count.Load() != 1 {
		t.Fatal("address reuse rotated transport")
	}
	nm.Stop()
}

func TestNetworkMonitorIntegration_MultipleClusters(t *testing.T) {
	a, ma, ca := monitorFixture(t, monitorState("en0"), nil)
	b, mb, cb := monitorFixture(t, monitorState("en0"), nil)
	// Existing evidence must survive local/peer address churn and unrelated NICs.
	for _, nm := range []*Runtime{a, b} {
		nm.transport.heartbeatStats.MarkReply()
	}
	lastA, lastB := a.LastHeartbeat(), b.LastHeartbeat()
	next := monitorState("en0")
	delete(next.Interface, "utun-peer")
	delete(next.InterfaceIPs, "utun-peer")
	next.InterfaceIPs["utun-own"] = nil
	next.InterfaceIPs["unrelated"] = []netip.Prefix{netip.MustParsePrefix("10.0.0.1/8")}
	ma.emit(next, false)
	mb.emit(next, false)
	monitorQuiet(t, a, lastA)
	monitorQuiet(t, b, lastB)
	// A real default-path change asks for evidence, not immediate rotation.
	ma.emit(monitorState("en1"), false)
	monitorWait(t, func() bool { return a.LastHeartbeat().IsZero() })
	if !b.LastHeartbeat().Equal(lastB) || ca.Load() != 1 || cb.Load() != 1 {
		t.Fatal("network change rotated or affected another manager")
	}
	// Cosmetic latest events cannot overwrite a queued resume notification.
	a.transport.heartbeatStats.MarkReply()
	ma.emit(monitorState("en1"), true)
	for range 100 {
		ma.emit(monitorState("en1"), false)
	}
	monitorWait(t, func() bool { return a.LastHeartbeat().IsZero() })
	a.Stop()
	b.Stop()
	transportAwait(t, ma.closed)
	transportAwait(t, mb.closed)
	ma.emit(monitorState("en0"), true)
	if ca.Load() != 1 || cb.Load() != 1 {
		t.Fatal("stop/event changed generation")
	}
}

func TestNetworkMonitorIntegration_FilterAndReorderedMailbox(t *testing.T) {
	nm, m, _ := monitorFixture(t, monitorState("en0"), nil)
	initial := monitorState("en0")
	initial.InterfaceIPs["en0"] = []netip.Prefix{netip.MustParsePrefix("192.0.2.1/24"), netip.MustParsePrefix("2001:db8::1/64")}
	m.emit(initial, false)
	monitorWait(t, func() bool {
		nm.transport.monitor.mu.Lock()
		defer nm.transport.monitor.mu.Unlock()
		return len(nm.transport.monitor.snapshot.interfaces["en0"].ips) == 2
	})
	nm.transport.heartbeatStats.MarkReply()
	last := nm.LastHeartbeat()
	reordered := monitorState("en0")
	reordered.InterfaceIPs["en0"] = []netip.Prefix{initial.InterfaceIPs["en0"][1], initial.InterfaceIPs["en0"][0]}
	reordered.HTTPProxy = "http://unused"
	reordered.PAC = "http://unused/pac"
	m.emit(reordered, false)
	monitorQuiet(t, nm, last)
	// A missing embedded net.Interface is safe and a selected-path change.
	nilIface := monitorState("en0")
	nilIface.Interface["en0"] = netmon.Interface{}
	m.emit(nilIface, false)
	monitorWait(t, func() bool { return nm.LastHeartbeat().IsZero() })
	// Simulate callbacks running out of source order. The final source is en1;
	// old cosmetic callbacks must not hide the current state or a sticky resume.
	m.mu.Lock()
	callback := m.callback
	m.state = monitorState("en1")
	m.mu.Unlock()
	nm.transport.heartbeatStats.MarkReply()
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			callback(&netmon.ChangeDelta{Old: monitorState("en0"), New: monitorState("en0"), TimeJumped: i == 0})
		}(i)
	}
	wg.Wait()
	monitorWait(t, func() bool { return nm.LastHeartbeat().IsZero() })
	monitorWait(t, func() bool {
		nm.transport.monitor.mu.Lock()
		defer nm.transport.monitor.mu.Unlock()
		return nm.transport.monitor.snapshot.defaultInterface == "en1"
	})
	nm.Stop()
	callback(&netmon.ChangeDelta{Old: initial, New: nilIface, TimeJumped: true}) // already-scheduled netmon callback
}

func TestNetworkMonitorIntegration_SSHAndProxyPathSelection(t *testing.T) {
	for _, tc := range []struct {
		name        string
		conf        *ssh.SshConfig
		wantDefault bool
	}{
		{"literal", &ssh.SshConfig{Addr: "203.0.113.8:22"}, false},
		{"alias", &ssh.SshConfig{ConfigAlias: "not-a-DNS-name"}, true},
		{"loopback", &ssh.SshConfig{Addr: "127.0.0.1:22"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, count := transportForwardServer(t, nil)
			nm := transportManager(t, srv.URL)
			nm.transport.launchTransportSupervisor()
			ctx, cancel := context.WithTimeout(nm.transport.ctx, 3*time.Second)
			defer cancel()
			if _, err := nm.transport.waitTransport(ctx); err != nil {
				t.Fatal(err)
			}
			// The already-running PF supervisor remains unchanged; configure just the
			// monitor's immutable view, as with an SSH-backed loopback API endpoint.
			nm.cfg.SshConf = tc.conf
			nm.cfg.APIServerIPs = []net.IP{net.ParseIP("127.0.0.1")}
			nm.cfg.Config.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
			r := &monitorRoutes{iface: "utun-corporate", gateway: "192.0.2.1", src: "192.0.2.3"}
			nm.routeTable.newRouteResolver = func() routeResolver { return r }
			m := &testNetworkMonitor{state: monitorState("en0"), started: make(chan struct{}), closed: make(chan struct{})}
			nm.transport.newMonitor = func(logger.Logf) (networkMonitorSource, error) { return m, nil }
			nm.transport.startNetworkMonitor(nm.device.tunName)
			transportAwait(t, m.started)
			nm.transport.heartbeatStats.MarkReply()
			last := nm.LastHeartbeat()
			m.emit(monitorState("en1"), false)
			if tc.wantDefault {
				monitorWait(t, func() bool { return nm.LastHeartbeat().IsZero() })
				if len(nm.transport.monitor.endpoints) != 0 || r.calls.Load() == 0 {
					t.Fatal("unknown endpoint must use representative default routes, not loopback/alias as WAN")
				}
			} else {
				monitorQuiet(t, nm, last)
				if r.calls.Load() == 0 {
					t.Fatal("SSH endpoint route not inspected")
				}
			}
			nm.transport.heartbeatStats.MarkReply()
			proxy := monitorState("en1")
			proxy.HTTPProxy = "http://changed"
			proxy.PAC = "http://changed/pac"
			m.emit(proxy, false)
			monitorWait(t, func() bool { return nm.LastHeartbeat().IsZero() })
			if count.Load() != 1 {
				t.Fatal("path selection rotated healthy PF")
			}
			nm.Stop()
		})
	}
}

func TestNetworkMonitorIntegration_ActualPathAndRouteOnly(t *testing.T) {
	r := &monitorRoutes{iface: "utun-corporate", gateway: "192.0.2.1", src: "192.0.2.3"}
	nm, m, count := monitorFixture(t, monitorState("en0"), r)
	nm.transport.heartbeatStats.MarkReply()
	last := nm.LastHeartbeat()
	// The API is on a corporate VPN, not the changing default path.
	m.emit(monitorState("en1"), false)
	monitorQuiet(t, nm, last)
	next := monitorState("en1")
	next.InterfaceIPs["utun-corporate"] = []netip.Prefix{netip.MustParsePrefix("192.0.2.30/24")}
	m.emit(next, false)
	monitorWait(t, func() bool { return nm.LastHeartbeat().IsZero() })
	nm.transport.heartbeatStats.MarkReply()
	r.mu.Lock()
	r.gateway = "192.0.2.254"
	r.mu.Unlock()
	// netmon suppresses equal states: the existing supervisor tick must catch it.
	deadline := time.After(livenessCheckInterval + 2*time.Second)
	for !nm.LastHeartbeat().IsZero() {
		select {
		case <-deadline:
			t.Fatal("route-only update lost")
		case <-time.After(10 * time.Millisecond):
		}
	}
	nm.transport.heartbeatStats.MarkReply()
	last = nm.LastHeartbeat()
	m.emit(next, false)
	monitorQuiet(t, nm, last)
	if count.Load() != 1 {
		t.Fatal("healthy route change rotated transport")
	}
}

type monitorEchoGateway struct{}

func (monitorEchoGateway) Handle(ctx context.Context, c net.Conn) {
	defer c.Close()
	framed, err := wire.NewConn(ctx, c)
	if err != nil {
		return
	}
	buf := make([]byte, 65536)
	for {
		n, err := framed.Read(buf)
		if err != nil {
			return
		}
		if n < 29 || buf[1]>>4 != 4 {
			continue
		}
		ip := header.IPv4(buf[1:n])
		off := int(ip.HeaderLength())
		if n < 1+off+8 || ip.Protocol() != 1 {
			continue
		}
		icmp := header.ICMPv4(buf[1+off : n])
		if icmp.Type() != header.ICMPv4Echo {
			continue
		}
		src := ip.SourceAddress()
		ip.SetSourceAddress(ip.DestinationAddress())
		ip.SetDestinationAddress(src)
		icmp.SetType(header.ICMPv4EchoReply)
		icmp.SetChecksum(0)
		icmp.SetChecksum(header.ICMPv4Checksum(icmp, 0))
		ip.SetChecksum(0)
		ip.SetChecksum(^ip.CalculateChecksum())
		if _, err = framed.Write(buf[:n]); err != nil {
			return
		}
	}
}

// Real framed slots and echo payloads exercise correlation through real PF. The
// synthetic TUN uses loopback only for address lookup, never OS packet I/O. Core's
// probeWire tests separately verify the same echo contract with real gVisor.
func monitorHeartbeatClient(t *testing.T) (*Runtime, *testNetworkMonitor, *atomic.Int32) {
	t.Helper()
	oldCIDR := config.CIDR
	_, config.CIDR, _ = net.ParseCIDR("127.0.0.0/8")
	t.Cleanup(func() { config.CIDR = oldCIDR })
	ctx, cancel := context.WithCancel(context.Background())
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	backendDone := make(chan struct{})
	go func() {
		core.Serve(ctx, []core.Server{{Listener: backend, Handler: monitorEchoGateway{}}})
		close(backendDone)
	}()
	t.Cleanup(func() { cancel(); backend.Close(); transportAwait(t, backendDone) })
	srv, count := transportForwardServer(t, map[int]string{config.PortTCP: backend.Addr().String()})
	nm := transportManager(t, srv.URL)
	m := &testNetworkMonitor{state: monitorState("en0"), started: make(chan struct{}), closed: make(chan struct{})}
	nm.transport.newMonitor = func(logger.Logf) (networkMonitorSource, error) { return m, nil }
	nm.transport.startNetworkMonitor(nm.device.tunName)
	transportAwait(t, m.started)
	nm.transport.launchTransportSupervisor()
	waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
	defer waitCancel()
	if _, err := nm.transport.waitTransport(waitCtx); err != nil {
		t.Fatal(err)
	}
	ep := channel.New(256, 1500, tcpip.GetRandMacAddr())
	tunCtx, tunCancel := context.WithCancel(ctx)
	device := &transportPacketTUN{ep, tunCtx, tunCancel}
	handler := tunnel.TunHandler((&core.Forwarder{Addr: "manager", Transporter: &generationTransporter{nm.transport, plainTransportDialer{}, endpointTCP}, Connector: core.NewUDPOverTCPConnector()}).TunnelConfig(), core.NewRouteHub(), nm.transport.heartbeatStats)
	done := make(chan struct{})
	go func() { handler.Handle(nm.transport.ctx, device); close(done) }()
	t.Cleanup(func() { nm.Stop(); device.Close(); transportAwait(t, done); ep.Close() })
	nm.transport.armTransportHealth()
	if err := nm.transport.waitTransportHealthy(waitCtx); err != nil {
		t.Fatal(err)
	}
	return nm, m, count
}

func TestNetworkMonitorIntegration_QueuedResumePrecedesStaleVerdict(t *testing.T) {
	nm, m, count := monitorFixture(t, monitorState("en0"), nil)
	g, err := nm.transport.waitTransport(nm.transport.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var stale atomic.Bool
	nm.transport.lastReply = func() time.Time {
		if stale.Load() {
			return time.Now().Add(-time.Hour)
		}
		return time.Now()
	}
	nm.transport.armTransportHealth()
	// Ensure the running monitor has seen fresh health and primed.
	nm.transport.requestNetworkCheck()
	select {
	case <-time.After(80 * time.Millisecond):
	case <-nm.transport.ctx.Done():
		t.Fatal("unexpected cancellation")
	}
	// Hold the publication gate while the resume is queued and old health ages.
	// On wake the supervisor must not retire before consuming the network hint.
	nm.transport.mu.Lock()
	stale.Store(true)
	m.emit(monitorState("en0"), true)
	nm.transport.mu.Unlock()
	select {
	case <-g.ctx.Done():
		t.Fatal("queued resume lost to stale evidence")
	case <-time.After(100 * time.Millisecond):
	}
	stale.Store(false)
	nm.transport.requestNetworkCheck()
	nm.Stop()
	if count.Load() != 1 {
		t.Fatal("resume immediately rotated transport")
	}
}

func TestNetworkMonitorIntegration_FreshEchoKeepsHealthyGeneration(t *testing.T) {
	nm, m, count := monitorHeartbeatClient(t)
	before := nm.transport.heartbeatStats.Snapshot()
	m.emit(monitorState("en1"), false)
	monitorWait(t, func() bool { return nm.transport.heartbeatStats.Snapshot().Replies > before.Replies })
	// Repeated cosmetic notifications cannot continuously clear fresh evidence.
	for range 50 {
		m.emit(monitorState("en1"), false)
	}
	select {
	case <-time.After(100 * time.Millisecond):
	case <-nm.transport.ctx.Done():
		t.Fatal("healthy change canceled tunnel")
	}
	if nm.LastHeartbeat().IsZero() || nm.transport.heartbeatStats.Generation() != before.Generation || count.Load() != 1 {
		t.Fatal("fresh echo failed to preserve healthy transport")
	}
}

func TestNetworkMonitorIntegration_UnhealthyChangeRecoversOnlyAffectedManager(t *testing.T) {
	old := livenessSteadyThreshold
	livenessSteadyThreshold = 100 * time.Millisecond
	defer func() { livenessSteadyThreshold = old }()
	a, ma, ca := monitorFixture(t, monitorState("en0"), nil)
	b, _, cb := monitorFixture(t, monitorState("en0"), nil)
	a.transport.armTransportHealth()
	g, err := a.transport.waitTransport(a.transport.ctx)
	if err != nil {
		t.Fatal(err)
	}
	// No forged heartbeat can rescue this real PF session: it has no echo gateway.
	ma.emit(monitorState("en1"), false)
	next := waitNextTransport(t, a, g.id)
	if next.id != g.id+1 || ca.Load() != 2 || cb.Load() != 1 || b.transport.heartbeatStats.Generation() != 0 {
		t.Fatal("recovery was not scoped to the unhealthy manager")
	}
	a.Stop()
	b.Stop()
}

func TestNetworkMonitorIntegration_NoDefaultRouteBackoffAndReturn(t *testing.T) {
	nm, m, count := monitorFixture(t, monitorState("en0"), nil)
	g, err := nm.transport.waitTransport(nm.transport.ctx)
	if err != nil {
		t.Fatal(err)
	}
	m.emit(monitorState(""), false)
	// Synchronize consumption, then fail the real PF session. Offline retry must
	// not churn at the ordinary 200ms reconnect rate.
	nm.transport.heartbeatStats.MarkReply()
	m.emit(monitorState(""), true)
	monitorWait(t, func() bool { return nm.LastHeartbeat().IsZero() })
	g.handles[0].Stop()
	select {
	case <-time.After(600 * time.Millisecond):
	case <-nm.transport.ctx.Done():
		t.Fatal("offline canceled manager")
	}
	if count.Load() != 1 {
		t.Fatal("no-default-route caused busy redial")
	}
	m.emit(monitorState("en1"), false)
	next := waitNextTransport(t, nm, g.id)
	c, err := nm.transport.dialGeneration(nm.transport.ctx, next, endpointTCP, plainTransportDialer{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	transportEcho(t, c)
	if count.Load() != 2 {
		t.Fatal("meaningful network return did not wake recovery")
	}
}

func TestNetworkMonitorIntegration_StopJoinsAndConstructorFailure(t *testing.T) {
	nm, m, _ := monitorFixture(t, monitorState("en0"), nil)
	gate := make(chan struct{})
	m.closeGate = gate
	done := make(chan struct{})
	go func() { nm.Stop(); close(done) }()
	transportAwait(t, nm.transport.ctx.Done())
	select {
	case <-done:
		t.Fatal("Stop did not join monitor close")
	default:
	}
	close(gate)
	transportAwait(t, done)
	srv, count := transportForwardServer(t, nil)
	failed := transportManager(t, srv.URL)
	failed.transport.newMonitor = func(logger.Logf) (networkMonitorSource, error) { return nil, errors.New("monitor unavailable") }
	failed.transport.startNetworkMonitor(failed.device.tunName)
	failed.transport.launchTransportSupervisor()
	ctx, cancel := context.WithTimeout(failed.transport.ctx, time.Second)
	defer cancel()
	if _, err := failed.transport.waitTransport(ctx); err != nil || count.Load() != 1 {
		t.Fatalf("monitor failure prevented watchdog: %v", err)
	}
	failed.Stop()
}
