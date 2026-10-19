package handler

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"testing"

	"github.com/containernetworking/cni/pkg/types"
	logrus "github.com/sirupsen/logrus"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// This file guards the failure mode described in docs/52-stale-route-src-after-ip-change.md:
// a connection that reports "connected", whose route table points every cluster destination at
// the TUN device, and which nevertheless carries no traffic in either direction — because the
// routes still source packets from a TUN address that was removed by an IP change.
//
// The routing table below models the one macOS behaviour that makes this possible: a route
// remembers the interface address present when it was installed (RTAX_IFA) and keeps using it
// as the packet source afterwards. With that one rule in place the whole bug — and the whole
// fix — reproduces on any platform, without root and without a real TUN device.
//
// Both address families are modelled, because the kernel pins them the same way and the repair
// path originally probed only IPv4.

// fakeRouteEntry is one kernel route: a destination prefix and the source address pinned into
// it at install time.
type fakeRouteEntry struct {
	dst net.IPNet
	src net.IP
}

// fakeRouteTable is the OS routing table as NetworkManager sees it through its three seams.
type fakeRouteTable struct {
	ifi *net.Interface
	// deviceV4/deviceV6 are the addresses the TUN device holds right now. Installing a route
	// copies the one matching its family; changing them afterwards does NOT update routes
	// already installed — that asymmetry is the bug.
	deviceV4 net.IP
	deviceV6 net.IP
	// linkLocal is the fe80:: address every device carries. macOS pins it into IPv6 routes as
	// their recorded source while packets are still sourced from the global address, so a check
	// that demanded equality with deviceV6 would call a healthy device broken forever.
	linkLocal net.IP
	entries   []fakeRouteEntry
	installs  int
}

// deviceAddrs is the seam NetworkManager uses to ask what the device currently holds.
func (f *fakeRouteTable) deviceAddrs(string) ([]net.IP, error) {
	addrs := []net.IP{f.deviceV4, f.deviceV6}
	if f.linkLocal != nil {
		addrs = append(addrs, f.linkLocal)
	}
	return addrs, nil
}

func newFakeRouteTable(tunName string, v4, v6 net.IP) *fakeRouteTable {
	return &fakeRouteTable{
		ifi:       &net.Interface{Index: 42, Name: tunName, MTU: 1500},
		deviceV4:  v4,
		deviceV6:  v6,
		linkLocal: net.ParseIP("fe80::a4e0:bb39:154f:25fe"),
	}
}

// deviceAddrFor returns the device address of ip's family — the source a route to ip would be
// stamped with if it were installed now.
func (f *fakeRouteTable) deviceAddrFor(ip net.IP) net.IP {
	if ip.To4() != nil {
		return f.deviceV4
	}
	return f.deviceV6
}

// Route resolves dst by longest prefix match, returning the pinned source address.
func (f *fakeRouteTable) Route(dst net.IP) (*net.Interface, net.IP, net.IP, error) {
	best := -1
	bestOnes := -1
	for i, e := range f.entries {
		if !e.dst.Contains(dst) {
			continue
		}
		if ones, _ := e.dst.Mask.Size(); ones > bestOnes {
			best, bestOnes = i, ones
		}
	}
	if best < 0 {
		return nil, nil, nil, fmt.Errorf("no route to %s", dst)
	}
	return f.ifi, nil, f.entries[best].src, nil
}

// AddRoutes installs or re-asserts routes, stamping each with the device's current address of
// the matching family — the same thing RTM_ADD and the RTM_CHANGE fallback do on a real kernel.
func (f *fakeRouteTable) AddRoutes(_ string, routes ...types.Route) error {
	for _, r := range routes {
		f.installs++
		src := f.deviceAddrFor(r.Dst.IP)
		if i := f.indexOf(r.Dst); i >= 0 {
			f.entries[i].src = src
			continue
		}
		f.entries = append(f.entries, fakeRouteEntry{dst: r.Dst, src: src})
	}
	return nil
}

func (f *fakeRouteTable) ListRoutes(string) ([]types.Route, error) {
	var out []types.Route
	for _, e := range f.entries {
		out = append(out, types.Route{Dst: e.dst})
	}
	return out, nil
}

func (f *fakeRouteTable) indexOf(dst net.IPNet) int {
	for i, e := range f.entries {
		if e.dst.String() == dst.String() {
			return i
		}
	}
	return -1
}

// staleSources returns the destinations whose recorded source is a dead address: for IPv4 one
// that is not the device's current address, for IPv6 one the device no longer holds at all.
// The asymmetry mirrors srcIsCurrentTunIP, and for the same measured reason.
func (f *fakeRouteTable) staleSources() []string {
	current, _ := f.deviceAddrs("")
	var bad []string
	for _, e := range f.entries {
		var ok bool
		if e.dst.IP.To4() != nil {
			ok = e.src.Equal(f.deviceV4)
		} else {
			for _, a := range current {
				if a.Equal(e.src) {
					ok = true
					break
				}
			}
		}
		if !ok {
			bad = append(bad, fmt.Sprintf("%s(src=%s)", e.dst.String(), e.src))
		}
	}
	sort.Strings(bad)
	return bad
}

// pinSources rewrites the remembered source of every route of ip's family, simulating routes
// that were installed while the device still held that address.
func (f *fakeRouteTable) pinSources(ip net.IP) {
	for i, e := range f.entries {
		if (e.dst.IP.To4() != nil) == (ip.To4() != nil) {
			f.entries[i].src = ip
		}
	}
}

// attach wires the table into nm as all three routing seams.
func (f *fakeRouteTable) attach(nm *NetworkManager) {
	nm.newRouteResolver = func() routeResolver { return f }
	nm.addTunRoutes = f.AddRoutes
	nm.listTunRoutes = f.ListRoutes
	nm.tunDeviceAddrs = f.deviceAddrs
}

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("ParseCIDR(%s): %v", s, err)
	}
	return n
}

// repairWithLog runs the watchdog's repair against a fresh logger and returns what it said.
func repairWithLog(t *testing.T, nm *NetworkManager) string {
	t.Helper()
	staleRouteSrcWarn.Reset()
	var buf strings.Builder
	nm.repairStaleRouteSrc(plog.WithLogger(context.Background(),
		plog.GetLoggerForClient(int32(logrus.InfoLevel), &buf)))
	return buf.String()
}

// TestIntegration_RouteSrc_SurvivesTunIPChange walks the whole incident, dual stack: a
// connection renting real addresses over gRPC, installing routes from all three sources, having
// its TUN addresses replaced, and then recovering — first through the refresh ChangeTunIP
// performs, then through the watchdog's repair for the case where nobody refreshed at all.
func TestIntegration_RouteSrc_SurvivesTunIPChange(t *testing.T) {
	ctx := context.Background()
	env := newNetworkTestEnv(t)
	nm := newTestNetworkManager(t, env, "owner-route-src")

	// Phase 1: rent TUN addresses over the real gRPC TunConfigService, then bring up the device.
	if err := nm.rentIP(ctx); err != nil {
		t.Fatalf("rentIP: %v", err)
	}
	if nm.localTunIPv6 == nil {
		t.Fatal("rentIP did not allocate an IPv6 address; the dual-stack legs below are the point")
	}
	oldV4, oldV6 := nm.localTunIPv4.IP, nm.localTunIPv6.IP
	nm.tunName = "utun-test"
	nm.cfg.CIDRs = []*net.IPNet{mustCIDR(t, "10.42.0.0/16"), mustCIDR(t, "2001:db8:42::/64")}

	table := newFakeRouteTable(nm.tunName, oldV4, oldV6)
	table.attach(nm)

	// Phase 2: install routes from all three sources, in both families — the cluster CIDRs laid
	// down at connect, /32 and /128 host routes, and a pod prefix pushed by the route watcher.
	for _, c := range nm.cfg.CIDRs {
		if err := nm.addRoutes(types.Route{Dst: *c}); err != nil {
			t.Fatalf("install cluster CIDR %s: %v", c, err)
		}
	}
	if err := nm.AddRoute("10.96.0.10", "2001:db8:96::10"); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if err := nm.AddRouteCIDR("10.244.3.0/24"); err != nil {
		t.Fatalf("AddRouteCIDR: %v", err)
	}
	if len(table.entries) != 5 {
		t.Fatalf("expected 5 routes installed, got %d: %v", len(table.entries), table.entries)
	}
	if bad := table.staleSources(); len(bad) != 0 {
		t.Fatalf("freshly installed routes already stale: %v", bad)
	}

	// Phase 3: a healthy re-announcement must not reinstall anything. This is the behaviour the
	// src check has to preserve — without it the route watcher would churn the table on every push.
	before := table.installs
	if err := nm.AddRoute("10.96.0.10", "2001:db8:96::10"); err != nil {
		t.Fatalf("AddRoute (repeat): %v", err)
	}
	if err := nm.AddRouteCIDR("10.244.3.0/24"); err != nil {
		t.Fatalf("AddRouteCIDR (repeat): %v", err)
	}
	if table.installs != before {
		t.Fatalf("already-correct routes were reinstalled: %d extra installs", table.installs-before)
	}

	// Phase 4: both TUN addresses change. The device holds the new ones; every route still
	// sources from the old. This is the state the user's machine was found in.
	newV4 := net.ParseIP("198.18.0.77").To4()
	newV6 := net.ParseIP("2001:2::77")
	nm.localTunIPv4 = &net.IPNet{IP: newV4, Mask: net.CIDRMask(32, 32)}
	nm.localTunIPv6 = &net.IPNet{IP: newV6, Mask: net.CIDRMask(128, 128)}
	table.deviceV4, table.deviceV6 = newV4, newV6
	if bad := table.staleSources(); len(bad) != 5 {
		t.Fatalf("expected all 5 routes stale after the IP change, got %v", bad)
	}

	// Phase 5: the refresh ChangeTunIP runs must cover every route in both families, not just
	// the cluster CIDRs — the host routes and the watcher-pushed prefix were what the previous
	// fix missed.
	nm.refreshTunRoutes(ctx)
	if bad := table.staleSources(); len(bad) != 0 {
		t.Fatalf("refreshTunRoutes left routes sourcing from a dead address: %v", bad)
	}

	// Phase 6: regress to the stale state and let the route watcher push the same prefixes
	// again. They must no longer be treated as "already installed" — that short circuit is what
	// made the outage permanent once a refresh had been missed.
	table.pinSources(oldV4)
	table.pinSources(oldV6)
	before = table.installs
	if err := nm.AddRouteCIDR("10.244.3.0/24"); err != nil {
		t.Fatalf("AddRouteCIDR (stale src): %v", err)
	}
	if err := nm.AddRoute("10.96.0.10", "2001:db8:96::10"); err != nil {
		t.Fatalf("AddRoute (stale src): %v", err)
	}
	if table.installs == before {
		t.Fatal("stale-source routes were skipped as already installed")
	}
	for _, dst := range []string{"10.244.3.0/24", "10.96.0.10/32", "2001:db8:96::10/128"} {
		i := table.indexOf(*mustCIDR(t, dst))
		if i < 0 {
			t.Fatalf("route %s disappeared", dst)
		}
		if want := table.deviceAddrFor(table.entries[i].dst.IP); !table.entries[i].src.Equal(want) {
			t.Fatalf("route %s still sources from %s, want %s", dst, table.entries[i].src, want)
		}
	}

	// Phase 7: nobody refreshed — the data-plane watchdog must notice and repair it on its own,
	// and must say so in the log rather than healing silently.
	table.pinSources(oldV4)
	table.pinSources(oldV6)
	out := repairWithLog(t, nm)
	if bad := table.staleSources(); len(bad) != 0 {
		t.Fatalf("watchdog repair left stale routes: %v", bad)
	}
	if !strings.Contains(out, oldV4.String()) || !strings.Contains(out, newV4.String()) {
		t.Fatalf("repair must name both the dead and the current address, got: %q", out)
	}

	// Phase 8: the IPv6 routes record the device's link-local address. This is the normal,
	// healthy state on macOS — the kernel pins fe80:: into IPv6 routes and picks the real source
	// at transmit — and an earlier version of this check called it broken, which would have made
	// the watchdog cry "dead source address" at a working tunnel on every teardown and made the
	// route watcher reinstall every IPv6 route on every push.
	for i, e := range table.entries {
		if e.dst.IP.To4() == nil {
			table.entries[i].src = table.linkLocal
		}
	}
	if bad := table.staleSources(); len(bad) != 0 {
		t.Fatalf("a link-local source on an IPv6 route is healthy, flagged: %v", bad)
	}
	before = table.installs
	out = repairWithLog(t, nm)
	if table.installs != before {
		t.Fatalf("link-local IPv6 sources triggered %d reinstalls", table.installs-before)
	}
	if out != "" {
		t.Fatalf("link-local IPv6 sources produced a warning: %q", out)
	}
	if err := nm.AddRouteCIDR("2001:db8:42::/64"); err != nil {
		t.Fatalf("AddRouteCIDR (link-local src): %v", err)
	}
	if table.installs != before {
		t.Fatalf("route watcher reinstalled an IPv6 route whose source is merely link-local")
	}

	// Phase 9: an IPv6 route recording an address the device no longer holds is the real
	// failure, and membership still catches it.
	table.pinSources(oldV6)
	if bad := table.staleSources(); len(bad) == 0 {
		t.Fatal("precondition: an IPv6 source that left the device should be stale")
	}
	out = repairWithLog(t, nm)
	if bad := table.staleSources(); len(bad) != 0 {
		t.Fatalf("IPv6-only staleness was not repaired: %v", bad)
	}
	if !strings.Contains(out, oldV6.String()) {
		t.Fatalf("repair must name the dead IPv6 address, got: %q", out)
	}
	if strings.Contains(out, newV4.String()) {
		t.Fatalf("repair blamed IPv4, which was healthy: %q", out)
	}

	// Phase 10: a healthy table must not trip the repair, or the warning becomes noise.
	before = table.installs
	out = repairWithLog(t, nm)
	if table.installs != before {
		t.Fatalf("repair reinstalled %d routes on a healthy table", table.installs-before)
	}
	if out != "" {
		t.Fatalf("repair warned about a healthy table: %q", out)
	}
}
