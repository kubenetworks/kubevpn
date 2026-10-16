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

// fakeRouteEntry is one kernel route: a destination prefix and the source address pinned into
// it at install time.
type fakeRouteEntry struct {
	dst net.IPNet
	src net.IP
}

// fakeRouteTable is the OS routing table as NetworkManager sees it through its three seams.
type fakeRouteTable struct {
	ifi *net.Interface
	// deviceAddr is the address the TUN device holds right now. Installing a route copies it;
	// changing it afterwards does NOT update routes already installed — that asymmetry is the bug.
	deviceAddr net.IP
	entries    []fakeRouteEntry
	installs   int
}

func newFakeRouteTable(tunName string, deviceAddr net.IP) *fakeRouteTable {
	return &fakeRouteTable{
		ifi:        &net.Interface{Index: 42, Name: tunName, MTU: 1500},
		deviceAddr: deviceAddr,
	}
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

// AddRoutes installs or re-asserts routes, stamping each with the device's current address —
// the same thing RTM_ADD and the RTM_CHANGE fallback do on a real kernel.
func (f *fakeRouteTable) AddRoutes(_ string, routes ...types.Route) error {
	for _, r := range routes {
		f.installs++
		if i := f.indexOf(r.Dst); i >= 0 {
			f.entries[i].src = f.deviceAddr
			continue
		}
		f.entries = append(f.entries, fakeRouteEntry{dst: r.Dst, src: f.deviceAddr})
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

// staleSources returns the destinations whose pinned source is not want, newest last.
func (f *fakeRouteTable) staleSources(want net.IP) []string {
	var bad []string
	for _, e := range f.entries {
		if !e.src.Equal(want) {
			bad = append(bad, fmt.Sprintf("%s(src=%s)", e.dst.String(), e.src))
		}
	}
	sort.Strings(bad)
	return bad
}

// detachDeviceAddr simulates the address change itself: the device now holds newAddr, while
// every route installed earlier keeps pointing at the address that is gone.
func (f *fakeRouteTable) detachDeviceAddr(newAddr net.IP) { f.deviceAddr = newAddr }

// attach wires the table into nm as all three routing seams.
func (f *fakeRouteTable) attach(nm *NetworkManager) {
	nm.newRouteResolver = func() routeResolver { return f }
	nm.addTunRoutes = f.AddRoutes
	nm.listTunRoutes = f.ListRoutes
}

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("ParseCIDR(%s): %v", s, err)
	}
	return n
}

// TestIntegration_RouteSrc_SurvivesTunIPChange walks the whole incident: a connection renting a
// real IP over gRPC, installing routes from all three sources, having its TUN address replaced,
// and then recovering — first through the refresh ChangeTunIP performs, then through the
// watchdog's repair for the case where nobody refreshed at all.
func TestIntegration_RouteSrc_SurvivesTunIPChange(t *testing.T) {
	ctx := context.Background()
	env := newNetworkTestEnv(t)
	nm := newTestNetworkManager(t, env, "owner-route-src")

	// Phase 1: rent a TUN IP over the real gRPC TunConfigService, then bring up the device.
	if err := nm.rentIP(ctx); err != nil {
		t.Fatalf("rentIP: %v", err)
	}
	oldIP := nm.localTunIPv4.IP
	nm.tunName = "utun-test"
	nm.cfg.CIDRs = []*net.IPNet{mustCIDR(t, "10.42.0.0/16")}

	table := newFakeRouteTable(nm.tunName, oldIP)
	table.attach(nm)

	// Phase 2: install routes from all three sources — the cluster CIDR laid down at connect,
	// a /32 for a service IP, and a pod prefix pushed later by the route watcher.
	if err := nm.addRoutes(types.Route{Dst: *mustCIDR(t, "10.42.0.0/16")}); err != nil {
		t.Fatalf("install cluster CIDR: %v", err)
	}
	if err := nm.AddRoute("10.96.0.10"); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if err := nm.AddRouteCIDR("10.244.3.0/24"); err != nil {
		t.Fatalf("AddRouteCIDR: %v", err)
	}
	if len(table.entries) != 3 {
		t.Fatalf("expected 3 routes installed, got %d", len(table.entries))
	}
	if bad := table.staleSources(oldIP); len(bad) != 0 {
		t.Fatalf("fresh routes should source from %s, stale: %v", oldIP, bad)
	}

	// Phase 3: a healthy re-announcement must not reinstall anything. This is the behaviour the
	// src check has to preserve — without it the route watcher would churn the table on every push.
	before := table.installs
	if err := nm.AddRoute("10.96.0.10"); err != nil {
		t.Fatalf("AddRoute (repeat): %v", err)
	}
	if err := nm.AddRouteCIDR("10.244.3.0/24"); err != nil {
		t.Fatalf("AddRouteCIDR (repeat): %v", err)
	}
	if table.installs != before {
		t.Fatalf("already-correct routes were reinstalled: %d extra installs", table.installs-before)
	}

	// Phase 4: the TUN IP changes. The device holds the new address; every route still sources
	// from the old one. This is the state the user's machine was found in.
	newIP := net.ParseIP("198.18.0.77").To4()
	nm.localTunIPv4 = &net.IPNet{IP: newIP, Mask: net.CIDRMask(32, 32)}
	table.detachDeviceAddr(newIP)
	if bad := table.staleSources(newIP); len(bad) != 3 {
		t.Fatalf("expected all 3 routes stale after IP change, got %v", bad)
	}

	// Phase 5: the refresh ChangeTunIP runs must cover every route, not just the cluster CIDR —
	// the /32 and the watcher-pushed /24 were exactly what the previous fix missed.
	nm.refreshTunRoutes(ctx)
	if bad := table.staleSources(newIP); len(bad) != 0 {
		t.Fatalf("refreshTunRoutes left routes sourcing from a dead address: %v", bad)
	}

	// Phase 6: regress to the stale state and let the route watcher push the same prefix again.
	// It must no longer be treated as "already installed" — that short circuit is what made the
	// outage permanent once a refresh had been missed.
	table.detachDeviceAddr(newIP)
	for i := range table.entries {
		table.entries[i].src = oldIP
	}
	before = table.installs
	if err := nm.AddRouteCIDR("10.244.3.0/24"); err != nil {
		t.Fatalf("AddRouteCIDR (stale src): %v", err)
	}
	if err := nm.AddRoute("10.96.0.10"); err != nil {
		t.Fatalf("AddRoute (stale src): %v", err)
	}
	if table.installs == before {
		t.Fatal("stale-source routes were skipped as already installed")
	}
	for _, dst := range []string{"10.244.3.0/24", "10.96.0.10/32"} {
		i := table.indexOf(*mustCIDR(t, dst))
		if i < 0 {
			t.Fatalf("route %s disappeared", dst)
		}
		if !table.entries[i].src.Equal(newIP) {
			t.Fatalf("route %s still sources from %s, want %s", dst, table.entries[i].src, newIP)
		}
	}

	// Phase 7: nobody refreshed — the data-plane watchdog must notice and repair it on its own,
	// and must say so in the log rather than healing silently.
	for i := range table.entries {
		table.entries[i].src = oldIP
	}
	staleRouteSrcWarn.Reset()
	var buf strings.Builder
	logger := plog.GetLoggerForClient(int32(logrus.InfoLevel), &buf)
	nm.repairStaleRouteSrc(plog.WithLogger(ctx, logger))
	if bad := table.staleSources(newIP); len(bad) != 0 {
		t.Fatalf("watchdog repair left stale routes: %v", bad)
	}
	if !strings.Contains(buf.String(), oldIP.String()) || !strings.Contains(buf.String(), newIP.String()) {
		t.Fatalf("repair must name both the dead and the current address, got: %q", buf.String())
	}

	// Phase 8: a healthy table must not trip the repair, or the warning becomes noise.
	before = table.installs
	buf.Reset()
	staleRouteSrcWarn.Reset()
	nm.repairStaleRouteSrc(plog.WithLogger(ctx, logger))
	if table.installs != before {
		t.Fatalf("repair reinstalled %d routes on a healthy table", table.installs-before)
	}
	if buf.Len() != 0 {
		t.Fatalf("repair warned about a healthy table: %q", buf.String())
	}
}
