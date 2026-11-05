package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/libp2p/go-netroute"
	v1 "k8s.io/api/core/v1"
	apinetworkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v2 "k8s.io/client-go/kubernetes/typed/networking/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/status"

	"github.com/wencaiwulue/kubevpn/v2/pkg/cluster/discovery"
	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
)

type routeTable struct {
	// The four seams onto the OS routing table and the device's own addresses. Indirected together so the stale-source repair
	// path can be driven on any platform, without root and without a real TUN device; each
	// falls back to the real implementation when unset.
	//
	// newRouteResolver opens a *snapshot* of the table — go-netroute parses the RIB once in
	// New(), so a fresh one per batch is what keeps answers current.
	newRouteResolver func() routeResolver
	addTunRoutes     func(tunName string, routes ...types.Route) error
	listTunRoutes    func(tunName string) ([]types.Route, error)
	tunDeviceAddrs   func(tunName string) ([]net.IP, error)
}

// routeResolver is the slice of the OS routing table Runtime consults: for a destination,
// which interface the kernel would send it out of and which source address it would carry.
// *netroute.router satisfies it structurally.
type routeResolver interface {
	Route(dst net.IP) (iface *net.Interface, gateway, preferredSrc net.IP, err error)
}

// defaultRouteResolver reads the live OS routing table. It returns nil (rather than an error)
// when the table cannot be opened, because every caller's fallback for "cannot tell" is the
// same: do the work rather than skip it.
func defaultRouteResolver() routeResolver {
	r, err := netroute.New()
	if err != nil || r == nil {
		return nil
	}
	return r
}

// routes opens a routing-table snapshot, or nil if unavailable.
func (nm *routeTable) routes() routeResolver {
	if nm.newRouteResolver == nil {
		return defaultRouteResolver()
	}
	return nm.newRouteResolver()
}

// addRoutes installs routes on the TUN device.
func (nm *routeTable) addRoutes(tunName string, routes ...types.Route) error {
	if nm.addTunRoutes == nil {
		return tun.AddRoutes(tunName, routes...)
	}
	return nm.addTunRoutes(tunName, routes...)
}

// listRoutes reports the routes kubevpn installed on the TUN device.
func (nm *routeTable) listRoutes(tunName string) ([]types.Route, error) {
	if nm.listTunRoutes == nil {
		return tun.ListRoutes(tunName)
	}
	return nm.listTunRoutes(tunName)
}

// staleRouteSrcWarn rate-limits the stale-source report. The watchdog can fire every 30s for
// hours; one line per occurrence would bury the log the way docs/51 describes.
var staleRouteSrcWarn = plog.NewThrottle(5 * time.Minute)

// repairStaleRouteSrc checks whether traffic to the cluster would still leave with the TUN's
// current address and re-asserts the routes if it would not.
//
// This is the self-healing half of the stale-source fix: ChangeTunIP already refreshes the
// routes when it changes the IP, but that only covers the transitions we perform ourselves.
// Anything else that detaches the address — and any route source we have not thought of —
// lands here instead of going silently dark for hours, which is how this bug was found.
// Callers gate this on macOS, the only platform that pins a source address into a route.
//
// Both address families are probed. They are renumbered by the same ChangeTunIP call and
// refreshed together, so a split is unlikely — but "unlikely asymmetry between v4 and v6" is
// exactly the shape of the bug in docs/51, where one path cached an address and its twin did
// not, and the half that still worked is what sent the diagnosis down the wrong road.
func (nm *Runtime) repairStaleRouteSrc(ctx context.Context) {
	if nm.device.tunName == "" || (nm.addresses.localTunIPv4 == nil && nm.addresses.localTunIPv6 == nil) {
		return
	}
	probes := nm.routeSrcProbeIPs()
	if len(probes) == 0 {
		return
	}
	r := nm.routeTable.routes()
	if r == nil {
		return
	}
	var stale bool
	for _, probe := range probes {
		ifi, _, src, err := r.Route(probe)
		if err != nil || ifi == nil || ifi.Name != nm.device.tunName {
			// Not routed through us at all — a different fault, and not one re-adding fixes.
			continue
		}
		if nm.srcIsCurrentTunIP(src, probe) {
			continue
		}
		stale = true
		// The two families fail in different ways, so they are reported differently — see
		// srcIsCurrentTunIP. Saying "sources from X" about an IPv6 route would be wrong: the
		// kernel picks that source at transmit, and what is broken is the recorded address.
		if probe.To4() != nil {
			staleRouteSrcWarn.Warnf(ctx, "stale-route-src-v4",
				"Route to %s via %s still sources from %s, but this TUN now holds %s — outbound "+
					"packets carry a dead address and every reply is dropped; re-asserting routes",
				probe, nm.device.tunName, src, nm.currentTunIP(probe))
		} else {
			staleRouteSrcWarn.Warnf(ctx, "stale-route-src-v6",
				"Route to %s via %s records source %s, which is no longer assigned to this TUN "+
					"(now %s); re-asserting routes",
				probe, nm.device.tunName, src, nm.currentTunIP(probe))
		}
	}
	// One refresh covers both families, so do it once however many probes complained.
	if stale {
		nm.refreshTunRoutes(ctx)
	}
}

// routeSrcProbeIPs picks one cluster destination per address family to ask the kernel about.
// Any address routed through the TUN answers "would our packets leave with the right source",
// so the first cluster CIDR of each family serves; they are present once Start has run.
func (nm *Runtime) routeSrcProbeIPs() []net.IP {
	var v4, v6 net.IP
	for _, cidr := range nm.dedupAndFilterCIDRs(nm.cfg.CIDRs) {
		if cidr == nil || cidr.IP.IsLoopback() {
			continue
		}
		if cidr.IP.To4() != nil {
			if v4 == nil {
				v4 = cidr.IP
			}
		} else if v6 == nil {
			v6 = cidr.IP
		}
	}
	var probes []net.IP
	if v4 != nil && nm.addresses.localTunIPv4 != nil {
		probes = append(probes, v4)
	}
	if v6 != nil && nm.addresses.localTunIPv6 != nil {
		probes = append(probes, v6)
	}
	return probes
}

// currentTunIP returns the TUN address of ip's family, for reporting. Nil-safe: a family we
// never allocated is not probed, so this is only reached with the matching address present.
func (nm *Runtime) currentTunIP(ip net.IP) net.IP {
	want := nm.addresses.localTunIPv4
	if ip.To4() == nil {
		want = nm.addresses.localTunIPv6
	}
	if want == nil {
		return nil
	}
	return want.IP
}

// dedupAndFilterCIDRs removes overlapping CIDRs and filters out those containing API server IPs.
func (nm *Runtime) dedupAndFilterCIDRs(cidrs []*net.IPNet) []*net.IPNet {
	return discovery.RemoveCIDRsContainingIPs(discovery.RemoveLargerOverlappingCIDRs(cidrs), nm.cfg.APIServerIPs)
}

// AddRoute adds IP addresses to the system route table via the TUN device,
// skipping any that match the API server or are already routed through the TUN.
func (nm *Runtime) AddRoute(ipStrList ...string) error {
	if nm.device.tunName == "" {
		return nil
	}
	var routes []types.Route
	r := nm.routeTable.routes()
	for _, ipStr := range ipStrList {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		var match bool
		for _, p := range nm.cfg.APIServerIPs {
			// if pod IP or service IP is equal to API server IP, cannot add it to route table
			if p.Equal(ip) {
				match = true
				break
			}
		}
		if match {
			continue
		}
		var mask net.IPMask
		if ip.To4() != nil {
			mask = net.CIDRMask(32, 32)
		} else {
			mask = net.CIDRMask(128, 128)
		}
		if nm.routedViaTunWithCurrentSrc(r, ip) {
			continue
		}
		routes = append(routes, types.Route{Dst: net.IPNet{IP: ip, Mask: mask}})
	}
	if len(routes) == 0 {
		return nil
	}
	return nm.routeTable.addRoutes(nm.device.tunName, routes...)
}

// addTrafficManagerServiceRoute adds /32 TUN routes for the traffic manager's
// ClusterIPs. Called synchronously during Start, before the async route watcher,
// so that dialManagerAddr's SYN travels through TUN from the start — preventing
// a mid-handshake route split (SYN via eth0, ACK via TUN) that makes gvisor
// RST the connection it never saw a SYN for.
func (nm *Runtime) addTrafficManagerServiceRoute(ctx context.Context) error {
	svc, err := nm.cfg.Clientset.CoreV1().Services(nm.cfg.ManagerNamespace).Get(
		ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return err
	}
	var ips []string
	if ip := svc.Spec.ClusterIP; ip != "" && ip != v1.ClusterIPNone {
		ips = append(ips, ip)
	}
	for _, ip := range svc.Spec.ClusterIPs {
		if ip != "" && ip != v1.ClusterIPNone {
			ips = append(ips, ip)
		}
	}
	return nm.AddRoute(ips...)
}

// routedViaTunWithCurrentSrc reports whether ip already routes through our TUN device *and*
// the kernel would source those packets from the TUN's current address.
//
// The second half is what makes this more than an "is it already installed?" check. On macOS
// the source address is pinned into the route when it is created, so after a TUN IP change a
// route can still point at the right device while sourcing from an address that no longer
// exists. Packets then leave carrying another client's identity, the server has no route back
// for it, and the whole connection goes dark while the route table still looks correct.
// Accepting such a route as "already installed" is what made that state unrecoverable.
func (nm *Runtime) routedViaTunWithCurrentSrc(r routeResolver, ip net.IP) bool {
	if r == nil {
		return false
	}
	ifi, _, src, err := r.Route(ip)
	if err != nil || ifi == nil || ifi.Name != nm.device.tunName {
		return false
	}
	return nm.srcIsCurrentTunIP(src, ip)
}

// srcIsCurrentTunIP reports whether a route's recorded source address is still good. The two
// families are judged differently because the kernels treat them differently:
//
//   - IPv4 demands equality with the TUN's current address. BSD hands the route's rt_ifa
//     straight to ip_output, so that address is literally what packets will carry, and nothing
//     weaker catches the case in docs/45 where the dead address is still on the device.
//   - IPv6 only demands that the address still exists on the device. macOS pins the interface's
//     *link-local* address into IPv6 routes and selects the real source at transmit
//     (in6_selectsrc), so requiring equality with the TUN's global address would be
//     permanently false: every route-watcher push would reinstall every IPv6 route, and the
//     watchdog would cry "dead source address" at a perfectly healthy tunnel. Membership still
//     catches the thing that matters — a source address that has been removed.
//
// Measured, not assumed: on macOS an IPv6 route reports rt_ifa fe80::… while a socket to the
// same destination sources from 2001:2::…; on Linux both report the global address. See
// docs/52-stale-route-src-after-ip-change.md.
//
// A family we never allocated has nothing to compare against, so it reports true and leaves
// the caller's behaviour as it was before source checking existed.
func (nm *Runtime) srcIsCurrentTunIP(src, dst net.IP) bool {
	if dst.To4() == nil {
		if nm.addresses.localTunIPv6 == nil {
			return true
		}
		return src != nil && nm.tunHasAddr(src)
	}
	if nm.addresses.localTunIPv4 == nil {
		return true
	}
	return src != nil && src.Equal(nm.addresses.localTunIPv4.IP)
}

// tunHasAddr reports whether ip is currently assigned to the TUN device. Reads the OS each
// call so it follows ChangeTunIP; the caller sites are per-route-batch, not per-packet.
// When the device cannot be read it reports true: "cannot tell" must not turn into endless
// reinstalls, and a TUN that has really gone away is a different fault with its own path.
func (nm *Runtime) tunHasAddr(ip net.IP) bool {
	addrs, err := nm.deviceAddrs()
	if err != nil {
		return true
	}
	for _, a := range addrs {
		if a.Equal(ip) {
			return true
		}
	}
	return false
}

// deviceAddrs lists the addresses currently on the TUN device.
func (nm *Runtime) deviceAddrs() ([]net.IP, error) {
	if nm.routeTable.tunDeviceAddrs != nil {
		return nm.routeTable.tunDeviceAddrs(nm.device.tunName)
	}
	ifi, err := net.InterfaceByName(nm.device.tunName)
	if err != nil {
		return nil, err
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil, err
	}
	var out []net.IP
	for _, a := range addrs {
		if ipNet, ok := a.(*net.IPNet); ok {
			out = append(out, ipNet.IP)
		}
	}
	return out, nil
}

// refreshTunRoutes re-asserts every route the kernel currently holds on the TUN device.
//
// On macOS the kernel pins RT_IFA — the source address outbound packets carry — when a route
// is created, so after a TUN IP change every route installed earlier keeps sourcing from the
// address that was just removed. tun.AddRoutes re-asserts in place (RTM_ADD, falling back to
// RTM_CHANGE when the entry exists), so the route is never briefly absent the way a
// delete-then-add pair would leave it.
//
// The route list comes from the kernel rather than from a list of what we remember adding,
// because routes reach this device from four places — device creation, AddRoute, AddRouteCIDR
// and the `kubevpn route add` RPC — and a remembered list would quietly stop covering them.
// On Linux and Windows tun.ListRoutes reports nothing, making this a no-op.
func (nm *Runtime) refreshTunRoutes(ctx context.Context) {
	if nm.device.tunName == "" {
		return
	}
	routes, err := nm.routeTable.listRoutes(nm.device.tunName)
	if err != nil {
		plog.G(ctx).Warnf("[Runtime] list TUN routes for refresh failed: %v", err)
		return
	}
	if len(routes) == 0 {
		return
	}
	if err = nm.routeTable.addRoutes(nm.device.tunName, routes...); err != nil {
		plog.G(ctx).Warnf("[Runtime] refresh TUN routes after IP change failed: %v", err)
		return
	}
	plog.G(ctx).Debugf("[Runtime] re-asserted %d routes on %s", len(routes), nm.device.tunName)
}

// AddRouteCIDR adds CIDR prefixes (e.g. server-pushed aggregated pod prefixes like
// "10.244.3.0/24") to the route table via the TUN device, skipping any that are
// invalid or already routed through the TUN. Unlike AddRoute (which builds /32 /128
// host routes from single IPs), this routes whole prefixes, matching how the cluster
// CIDRs are installed at connect.
func (nm *Runtime) AddRouteCIDR(cidrs ...string) error {
	if nm.device.tunName == "" {
		return nil
	}
	var routes []types.Route
	r := nm.routeTable.routes()
	for _, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil || ipNet == nil || ipNet.IP.IsLoopback() {
			continue
		}
		if nm.routedViaTunWithCurrentSrc(r, ipNet.IP) {
			continue
		}
		routes = append(routes, types.Route{Dst: *ipNet})
	}
	if len(routes) == 0 {
		return nil
	}
	return nm.routeTable.addRoutes(nm.device.tunName, routes...)
}

var errRouteDiscoveryDisabled = errors.New("server route discovery disabled (no RBAC); using CIDR-only routing")

// StartRouteWatcher subscribes to the traffic manager's WatchNamespaceRoutes stream
// for the workload namespace and applies pushed pod route prefixes (to the route
// table) and service records (to DNS), replacing the former client-side cluster-wide
// pod/service informers. It runs in the background under ctx; on any failure it
// degrades to CIDR-only routing (the cluster CIDRs are already routed).
func (nm *Runtime) StartRouteWatcher(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	nm.transport.startWatchWorker(ctx, nm.watchNamespaceRoutes)
}

func (nm *Runtime) watchNamespaceRoutes(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	plog.G(ctx).Infof("[RouteWatcher] Starting route watcher for namespace %q at %s", nm.cfg.WorkloadNamespace, managerTarget)
	warnings := plog.NewThrottle(time.Minute)
	var version int64
	for ctx.Err() == nil {
		err := nm.doWatchNamespaceRoutes(ctx, &version)
		if err != nil && ctx.Err() == nil {
			if warnings.Allow("retry") {
				plog.G(ctx).Warnf("[RouteWatcher] %s (namespace %q) disconnected: %v, retrying in %s", managerTarget, nm.cfg.WorkloadNamespace, err, ipWatcherRetryInterval)
			} else {
				plog.G(ctx).Debugf("[RouteWatcher] %s (namespace %q) disconnected: %v, retrying in %s", managerTarget, nm.cfg.WorkloadNamespace, err, ipWatcherRetryInterval)
			}
		}
		select {
		case <-time.After(ipWatcherRetryInterval):
		case <-ctx.Done():
			return
		}
	}
}

func (nm *Runtime) doWatchNamespaceRoutes(ctx context.Context, version *int64) error {
	ctx, cancel, g, err := nm.transport.watchGeneration(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	target := g.endpoints.xds
	plog.G(ctx).Debugf("[RouteWatcher] Attempting to dial xDS at %s (namespace %q, timeout %s)", target, nm.cfg.WorkloadNamespace, watcherDialTimeout)
	started := time.Now()
	dialCtx, dialCancel := context.WithTimeout(ctx, watcherDialTimeout)
	opts := append(nm.transport.watchXDSOptions(g), grpc.WithDefaultCallOptions(grpc.UseCompressor(gzip.Name)), grpc.WithBlock())
	conn, err := grpc.DialContext(dialCtx, managerTarget, opts...)
	dialCancel()
	if err != nil {
		return fmt.Errorf("dial %s after %s: %w", target, time.Since(started), err)
	}
	defer conn.Close()
	plog.G(ctx).Debugf("[RouteWatcher] connected to xDS at %s in %s", target, time.Since(started))

	client := rpc.NewTunConfigServiceClient(conn)
	stream, err := client.WatchNamespaceRoutes(ctx, &rpc.NamespaceRoutesRequest{Namespace: nm.cfg.WorkloadNamespace})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			// Old traffic manager without server-side discovery: nothing to do, the
			// cluster CIDR routes already cover routing. Do not retry-spin.
			plog.G(ctx).Debugf("[RouteWatcher] manager has no WatchNamespaceRoutes; using CIDR-only routing")
			<-ctx.Done()
			return nil
		}
		return fmt.Errorf("watch namespace routes: %w", err)
	}

	// services accumulates the current service set across snapshot+delta frames; it is
	// the source fed to DNS on every change.
	services := map[string]*rpc.ServiceRecord{}
	for {
		resp, err := stream.Recv()
		if err != nil {
			return err
		}
		if !resp.Enabled {
			// Keep the existing routes/hosts and retry, so corrected permissions
			// become effective without recreating the entire VPN connection.
			return fmt.Errorf("namespace %q: %w", nm.cfg.WorkloadNamespace, errRouteDiscoveryDisabled)
		}
		if resp.Snapshot {
			plog.G(ctx).Infof("[RouteWatcher] Received snapshot for namespace %q: %d services, %d pod CIDRs", nm.cfg.WorkloadNamespace, len(resp.UpsertedServices), len(resp.AddedPodCIDRs))
		} else {
			plog.G(ctx).Debugf("[RouteWatcher] Received delta for namespace %q: version=%d, %d service upserts, %d service removals", nm.cfg.WorkloadNamespace, resp.Version, len(resp.UpsertedServices), len(resp.RemovedServiceKeys))
		}
		if err := nm.transport.applyGeneration(ctx, g, func() error {
			applyRouteFrame(resp, services,
				func(cidrs []string) {
					if err := nm.AddRouteCIDR(cidrs...); err != nil {
						plog.G(ctx).Debugf("[RouteWatcher] add pod CIDR routes failed: %v", err)
					}
				},
				func(ips []string) {
					if err := nm.AddRoute(ips...); err != nil {
						plog.G(ctx).Debugf("[RouteWatcher] add service IP routes failed: %v", err)
					}
				},
				func(svcs []v1.Service) {
					if nm.dns.dnsConfig != nil {
						nm.dns.dnsConfig.UpdateServices(ctx, svcs)
					}
				},
			)
			*version = resp.Version
			return nil
		}); err != nil {
			return err
		}
	}
}

// applyRouteFrame applies one WatchNamespaceRoutes frame to the running state: it resets
// the service set on a snapshot, adds pushed pod CIDR routes (via addCIDR), routes the
// upserted services' ClusterIPs (via addServiceIPs), upserts/removes service records, and
// pushes the updated service set to DNS (via setDNS) when it changed.
// Routes are add-only: RemovedPodCIDRs / RemovedServiceKeys are NOT unrouted (a stale route
// pointed at the TUN is harmless), they only affect state. Pure of gRPC/Runtime for
// unit-testing.
func applyRouteFrame(resp *rpc.NamespaceRoutesResponse, services map[string]*rpc.ServiceRecord,
	addCIDR func([]string), addServiceIPs func([]string), setDNS func([]v1.Service)) {
	if resp.Snapshot {
		for k := range services {
			delete(services, k)
		}
	}
	if len(resp.AddedPodCIDRs) > 0 {
		addCIDR(resp.AddedPodCIDRs)
	}
	// Route each upserted service's ClusterIPs. The whole service CIDR is normally routed at
	// connect, but that detection is best-effort and may miss/exclude ranges (managed clusters
	// hide component flags, the error-trick message may not match, or the range is dropped by
	// the API-server-IP filter). Without a per-service route the name still resolves (hosts
	// entry) but the connection has no path, so add the ClusterIPs explicitly — addServiceIPs
	// (nm.AddRoute) skips API server IPs and already-routed IPs and is idempotent.
	var svcIPs []string
	changed := resp.Snapshot
	for _, rec := range resp.UpsertedServices {
		services[rec.Namespace+"/"+rec.Name] = rec
		svcIPs = append(svcIPs, rec.ClusterIPs...)
		changed = true
	}
	if len(svcIPs) > 0 {
		addServiceIPs(svcIPs)
	}
	for _, key := range resp.RemovedServiceKeys {
		if _, ok := services[key]; ok {
			delete(services, key)
			changed = true
		}
	}
	if changed {
		setDNS(serviceRecordsToServices(services))
	}
}

// serviceRecordsToServices converts pushed ServiceRecords into the corev1.Service
// shape the DNS layer consumes (only the fields it reads: name, namespace, ClusterIP,
// ExternalName).
func serviceRecordsToServices(recs map[string]*rpc.ServiceRecord) []v1.Service {
	out := make([]v1.Service, 0, len(recs))
	for _, rec := range recs {
		svc := v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: rec.Name, Namespace: rec.Namespace},
			Spec:       v1.ServiceSpec{ExternalName: rec.ExternalName},
		}
		if len(rec.ClusterIPs) > 0 {
			svc.Spec.ClusterIP = rec.ClusterIPs[0]
			svc.Spec.ClusterIPs = rec.ClusterIPs
		}
		out = append(out, svc)
	}
	return out
}

// AddExtraRoute resolves extra domain names by querying the in-cluster DNS
// forward server (servers, populated by detectNameserver) and adds their IPs to
// the route table. It falls back to ingress records when DNS yields no answer.
func (nm *Runtime) AddExtraRoute(ctx context.Context, servers []string) error {
	if len(nm.cfg.RouteConfig.ExtraDomain) == 0 {
		return nil
	}

	for _, domain := range nm.cfg.RouteConfig.ExtraDomain {
		var ip string
		if resolved, err := resolveDomainViaClusterDNS(ctx, servers, domain); err == nil {
			ip = resolved.String()
		} else {
			// fall back to ingress record
			ip = getIngressRecord(ctx, nm.cfg.Clientset.NetworkingV1(), []string{v1.NamespaceAll, nm.cfg.ManagerNamespace}, domain)
		}
		if net.ParseIP(ip) == nil {
			return fmt.Errorf("failed to resolve DNS for domain %s via cluster DNS", domain)
		}
		if err := nm.AddRoute(ip); err != nil {
			plog.G(ctx).Errorf("Failed to add IP: %s to route table: %v", ip, err)
			return err
		}
		nm.dns.extraHost = append(nm.dns.extraHost, dns.Entry{IP: net.ParseIP(ip).String(), Domain: domain})
	}
	return nil
}

// AddExtraNodeIP adds cluster node IPs to the extra CIDR list so they
// get routed through the TUN device.
func (nm *Runtime) AddExtraNodeIP(ctx context.Context) error {
	if !nm.cfg.RouteConfig.ExtraNodeIP {
		return nil
	}
	list, err := nm.cfg.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, item := range list.Items {
		for _, address := range item.Status.Addresses {
			ip := net.ParseIP(address.Address)
			if ip != nil {
				var mask net.IPMask
				if ip.To4() != nil {
					mask = net.CIDRMask(32, 32)
				} else {
					mask = net.CIDRMask(128, 128)
				}
				nm.cfg.RouteConfig.ExtraCIDR = append(nm.cfg.RouteConfig.ExtraCIDR, (&net.IPNet{
					IP:   ip,
					Mask: mask,
				}).String())
			}
		}
	}
	return nil
}

// getIngressRecord searches ingress resources for a matching domain and returns
// its load balancer IP.
func getIngressRecord(ctx context.Context, ingressInterface v2.NetworkingV1Interface, nsList []string, domain string) string {
	var ingressList []apinetworkingv1.Ingress
	for _, ns := range nsList {
		list, err := ingressInterface.Ingresses(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			plog.G(ctx).Debugf("Failed to list ingresses in namespace %s: %v", ns, err)
			continue
		}
		ingressList = append(ingressList, list.Items...)
	}
	for _, item := range ingressList {
		for _, rule := range item.Spec.Rules {
			if rule.Host == domain {
				for _, ingress := range item.Status.LoadBalancer.Ingress {
					if ingress.IP != "" {
						return ingress.IP
					}
				}
			}
		}
		for _, tl := range item.Spec.TLS {
			if slices.Contains(tl.Hosts, domain) {
				for _, ingress := range item.Status.LoadBalancer.Ingress {
					if ingress.IP != "" {
						return ingress.IP
					}
				}
			}
		}
	}
	return ""
}
