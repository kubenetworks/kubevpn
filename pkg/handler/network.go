package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/libp2p/go-netroute"
	v1 "k8s.io/api/core/v1"
	apinetworkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	v2 "k8s.io/client-go/kubernetes/typed/networking/v1"
	"k8s.io/client-go/rest"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/status"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
	"github.com/wencaiwulue/kubevpn/v2/pkg/driver"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

// NetworkConfig holds immutable configuration for NetworkManager.
type NetworkConfig struct {
	Clientset         kubernetes.Interface
	RESTClient        *rest.RESTClient
	Config            *rest.Config
	ManagerNamespace  string
	WorkloadNamespace string
	CIDRs             []*net.IPNet
	APIServerIPs      []net.IP
	ExtraRouteInfo    *ExtraRouteInfo
	Image             string
	Lock              *sync.Mutex // shared lock for DNS operations
	OwnerID           string      // unique ID for TunConfigService (UUID)
	Hostname          string      // local machine name, recorded in TUN_ALLOCS for debugging

	// GetRunningPodList returns running traffic manager pods.
	GetRunningPodList func(ctx context.Context) ([]v1.Pod, error)

	// ReservedTunIPs returns TUN IPs already held by sibling connections (other
	// clusters) in the same daemon. They are excluded from this connection's
	// allocation so two clusters don't assign the same local TUN IP. Optional.
	ReservedTunIPs func() []net.IP
}

// NetworkManager owns the full networking lifecycle: port-forward, TUN, routes, DNS.
type NetworkManager struct {
	cfg NetworkConfig

	// Runtime state (set by Start, cleared by Stop)
	ctx                   context.Context
	cancel                context.CancelFunc
	localTunIPv4          *net.IPNet // allocated by RentIP, used by TUN/routes/DNS
	localTunIPv6          *net.IPNet
	tunName               string
	controlPlaneLocalPort int // local port for TunConfigService (port-forwarded from 9002)
	extraHost             []dns.Entry
	dnsConfig             *dns.Config
	heartbeatStats        *core.HeartbeatStats // data-plane liveness from observed heartbeat replies
}

const (
	// ipWatcherRetryInterval is the delay before reconnecting the cluster IP watcher.
	ipWatcherRetryInterval = 10 * time.Second
	// portForwardPodListTimeout bounds listing the traffic-manager pod before a port-forward session.
	portForwardPodListTimeout = 10 * time.Second
	// portForwardReconnectDelay is the initial/minimum pause between port-forward reconnect attempts.
	portForwardReconnectDelay = 200 * time.Millisecond
	// portForwardReconnectMaxDelay caps the exponential backoff between reconnects, so a
	// sustained apiserver stall is not hammered with rapid Pods.List + TLS-handshake retries.
	// Kept small: once a session is detected dead (now within ~10s, including the black-hole
	// case), the whole tunnel is down until the next session comes up, so a long reconnect gap
	// directly prolongs the outage. 2s still throttles a hard apiserver stall to ~30 retries/min.
	portForwardReconnectMaxDelay = 2 * time.Second
	// portForwardHealthySession is the minimum session duration considered "healthy": a
	// session that lasted at least this long before dropping resets the reconnect backoff
	// (normal pod recreation reconnects fast); shorter sessions grow the backoff.
	portForwardHealthySession = 30 * time.Second
	// portForwardStartTimeout is how long to wait for the first port-forward session to become ready.
	portForwardStartTimeout = 60 * time.Second
	// livenessCheckInterval is how often the data-plane liveness watchdog samples HeartbeatStats.
	livenessCheckInterval = 5 * time.Second
	// livenessStartupDeadline bounds how long a session may run without ever producing a fresh
	// heartbeat echo reply (never becoming ready, or ready-but-black-holed from the start) before it
	// is force-reconnected. Must be >> the time for a healthy reconnected session to prime a reply
	// (slot reconnect <= SlotReconnectBackoff + RTT), so a healthy session is never torn down for a
	// reply it has not had time to receive.
	livenessStartupDeadline = 30 * time.Second
	// rentIPCallTimeout bounds a single GetTunIP call so a black-holed port-forward during startup
	// (which no longer tears itself down via the xDS health check) makes the call time out and retry
	// instead of hanging forever. The watchdog reconnects the session underneath; a later attempt
	// then lands on a healthy session.
	rentIPCallTimeout = 10 * time.Second
)

// livenessSteadyThreshold is how long an already-primed session (one that produced a fresh heartbeat
// echo reply) may then go silent before it is treated as black-holed and force-reconnected. A small
// multiple of config.HeartbeatInterval so a couple of missed beats trip it. It is a var (not a const)
// because config.HeartbeatInterval is a runtime var.
var livenessSteadyThreshold = 3 * config.HeartbeatInterval

// newNetworkManager creates a NetworkManager with the given configuration.
func newNetworkManager(cfg NetworkConfig) *NetworkManager {
	nm := &NetworkManager{cfg: cfg}
	// Allocate here (once, before any goroutine) so the field is never nil and never re-assigned:
	// the data-plane liveness watchdog spawned by the first port-forward session reads it before
	// startTUN runs, and status queries read it concurrently. LastReply stays zero until the TUN
	// heartbeat sender populates it (the watchdog's startup deadline tolerates that window).
	nm.heartbeatStats = &core.HeartbeatStats{}
	return nm
}

// LocalTunIPv4 returns the IPv4 TUN address.
func (nm *NetworkManager) LocalTunIPv4() *net.IPNet { return nm.localTunIPv4 }

// LocalTunIPv6 returns the IPv6 TUN address.
func (nm *NetworkManager) LocalTunIPv6() *net.IPNet { return nm.localTunIPv6 }

// TunName returns the TUN device name (empty if not started).
func (nm *NetworkManager) TunName() string {
	return nm.tunName
}

// LastHeartbeat returns the time of the last observed ICMP echo reply from the server gateway,
// or the zero time if none has been seen. It is the data-plane liveness signal.
func (nm *NetworkManager) LastHeartbeat() time.Time {
	return nm.heartbeatStats.LastReply()
}

// GetExtraHost returns the extra DNS host entries accumulated by AddExtraRoute.
func (nm *NetworkManager) GetExtraHost() []dns.Entry {
	return nm.extraHost
}

// Start brings up the full networking stack in order:
// 1. Add extra node IPs to route info
// 2. Port-forward to traffic manager (gvisor TCP/UDP + xds)
// 3. Allocate TUN IP + create local TUN device with gvisor stack
// 4. Add dynamic routes (watch pods/services)
// 5. Configure DNS
func (nm *NetworkManager) Start(ctx context.Context) error {
	nm.ctx, nm.cancel = context.WithCancel(ctx)

	if err := nm.AddExtraNodeIP(nm.ctx); err != nil {
		return err
	}

	gvisorTCPForwardPort, err := util.GetAvailableTCPPort()
	if err != nil {
		return err
	}
	gvisorUDPForwardPort, err := util.GetAvailableTCPPort()
	if err != nil {
		return err
	}
	controlPlanePort, err := util.GetAvailableTCPPort()
	if err != nil {
		return err
	}
	nm.controlPlaneLocalPort = controlPlanePort

	plog.StepStart(nm.ctx, "Forwarding ports")
	portPair := []string{
		fmt.Sprintf("%d:%d", gvisorTCPForwardPort, config.PortTCP),
		fmt.Sprintf("%d:%d", gvisorUDPForwardPort, config.PortUDP),
		fmt.Sprintf("%d:%d", controlPlanePort, config.PortXDS),
	}

	if err := nm.portForward(nm.ctx, portPair); err != nil {
		return err
	}
	plog.StepDone(nm.ctx, "Forwarded ports (TCP/UDP/xDS)")

	if util.IsWindows() {
		driver.InstallWireGuardTunDriver()
	}

	forward := fmt.Sprintf("tcp://127.0.0.1:%d", gvisorTCPForwardPort)
	if err := nm.startTUN(nm.ctx, forward); err != nil {
		return fmt.Errorf("start tun: %w: %w", err, config.ErrTunDeviceFailed)
	}

	// Cluster pod/service CIDRs are already routed by startTUN. Per-IP pod routes and
	// service records now come from the traffic manager over WatchNamespaceRoutes
	// (StartRouteWatcher below), instead of a client-side cluster-wide list-watch.
	plog.StepDone(nm.ctx, "Added %d pod/service CIDR routes", len(nm.cfg.CIDRs))

	// Pre-route the traffic manager's ClusterIP through TUN so that dialManager
	// (server-side inject) sees a consistent path from SYN onward. Without this,
	// the route watcher adds the /32 asynchronously and can split a TCP connection
	// mid-handshake: SYN via pod network, ACK via TUN → gvisor RSTs the orphan ACK.
	if err := nm.addTrafficManagerServiceRoute(nm.ctx); err != nil {
		plog.G(nm.ctx).Debugf("Failed to pre-route traffic manager ClusterIP: %v", err)
	}

	if err := nm.setupDNS(nm.ctx); err != nil {
		return fmt.Errorf("setup dns: %w: %w", err, config.ErrDNSSetupFailed)
	}

	// Subscribe to server-side route/service discovery for the workload namespace.
	// Runs in the background under nm.ctx; failures degrade to CIDR-only routing.
	nm.StartRouteWatcher(nm.ctx)

	return nil
}

// rentIP allocates a TUN IP from the xds's TunConfigService via the
// already-established port-forward. Passes local interface IPs as ExcludeIPs
// so the server avoids conflicts. Retries on the rare race where a new
// interface appears between collecting addresses and receiving the allocation.
func (nm *NetworkManager) rentIP(ctx context.Context) error {
	target := fmt.Sprintf("127.0.0.1:%d", nm.controlPlaneLocalPort)
	// No WithBlock: the local port-forward listener may momentarily be down across a reconnect, so
	// each GetTunIP waits-for-ready within its own bounded timeout and retries, rather than blocking
	// the dial forever on a black-holed session.
	conn, err := grpc.DialContext(ctx, target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("dial xds for RentIP: %w", err)
	}
	defer conn.Close()

	client := rpc.NewTunConfigServiceClient(conn)

	const maxRetries = 15
	for i := 0; i < maxRetries; i++ {
		callCtx, callCancel := context.WithTimeout(ctx, rentIPCallTimeout)
		resp, err := client.GetTunIP(callCtx, &rpc.TunIPRequest{
			OwnerID:    nm.cfg.OwnerID,
			Namespace:  nm.cfg.ManagerNamespace,
			ExcludeIPs: buildExcludeIPs(nm.cfg.ReservedTunIPs),
			Hostname:   nm.cfg.Hostname,
		}, grpc.WaitForReady(true))
		callCancel()
		if err != nil {
			// A black-holed or not-yet-ready port-forward makes the call time out or the conn
			// unavailable; retry (the watchdog reconnects the session underneath us) until the
			// parent ctx is cancelled or retries are exhausted.
			if ctx.Err() != nil {
				return fmt.Errorf("get TUN IP from xds: %w", ctx.Err())
			}
			plog.G(ctx).Debugf("GetTunIP attempt %d/%d failed: %v; retrying", i+1, maxRetries, err)
			continue
		}

		ip4, cidr4, err := net.ParseCIDR(resp.IPv4)
		if err != nil || cidr4 == nil {
			return fmt.Errorf("invalid IPv4 from xds: %q", resp.IPv4)
		}
		v4 := &net.IPNet{IP: ip4, Mask: cidr4.Mask}

		if isLocalIPConflict(v4.IP) {
			plog.G(ctx).Infof("TUN IP %s conflicts with local interface (race), retrying (%d/%d)", v4.IP, i+1, maxRetries)
			continue
		}

		var v6 *net.IPNet
		if resp.IPv6 != "" {
			ip6, cidr6, _ := net.ParseCIDR(resp.IPv6)
			if cidr6 != nil {
				v6 = &net.IPNet{IP: ip6, Mask: cidr6.Mask}
			}
		}

		nm.localTunIPv4 = v4
		nm.localTunIPv6 = v6
		plog.G(ctx).Debugf("Allocated TUN IP: v4=%s v6=%s", v4, v6)
		return nil
	}

	return fmt.Errorf("failed to allocate a non-conflicting TUN IP after %d attempts: %w", maxRetries, config.ErrTunIPConflict)
}

// buildExcludeIPs returns the IPs the TUN allocator must avoid: this host's
// interface addresses plus any TUN IPs already held by sibling connections
// (other clusters), so the allocator never hands out a locally-conflicting IP.
func buildExcludeIPs(reserved func() []net.IP) []string {
	excludeIPs := collectLocalIPs()
	if reserved != nil {
		for _, ip := range reserved() {
			excludeIPs = append(excludeIPs, ip.String())
		}
	}
	return excludeIPs
}

func collectLocalIPs() []string {
	addrs, _ := net.InterfaceAddrs()
	ips := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok {
			ips = append(ips, ipNet.IP.String())
		}
	}
	return ips
}

func isLocalIPConflict(ip net.IP) bool {
	addrs, _ := net.InterfaceAddrs()
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// Stop tears down networking: cancels DNS, stops port-forward/informers, clears state.
func (nm *NetworkManager) Stop() {
	if nm.dnsConfig != nil {
		nm.dnsConfig.CancelDNS()
	}
	if nm.cancel != nil {
		nm.cancel()
	}
	nm.tunName = ""
	nm.dnsConfig = nil
	nm.extraHost = nil
}

// ChangeTunIP hot-updates the TUN device IP without restarting the network stack.
// The next heartbeat automatically uses the new IP (heartbeat reads from OS each tick).
func (nm *NetworkManager) ChangeTunIP(ctx context.Context, newIPv4, newIPv6 *net.IPNet) error {
	if nm.tunName == "" {
		return fmt.Errorf("TUN device not started: %w", config.ErrTunDeviceFailed)
	}
	if newIPv4 == nil {
		return fmt.Errorf("new IPv4 is nil: %w", config.ErrTunDeviceFailed)
	}

	oldV4, oldV6 := nm.localTunIPv4, nm.localTunIPv6
	// Always operate on host masks (/32, /128) — matching how startTUN created the
	// device. Leases now carry a host mask too (the TunConfigService stamps /32, /128),
	// so this is defensive: even if nm.localTunIPv4 ever carried a wider mask, forcing
	// /32 here keeps the delete/add aligned with the address actually on the device.
	// Only touch a family that actually changed.
	host32 := func(ip net.IP) string { return (&net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)}).String() }
	host128 := func(ip net.IP) string { return (&net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}).String() }

	if oldV4 == nil || !oldV4.IP.Equal(newIPv4.IP) {
		oldAddr := ""
		if oldV4 != nil {
			oldAddr = host32(oldV4.IP)
		}
		if err := tun.ChangeIP(nm.tunName, oldAddr, host32(newIPv4.IP)); err != nil {
			return fmt.Errorf("change IPv4 on %s: %w: %w", nm.tunName, err, config.ErrTunDeviceFailed)
		}
	}

	if newIPv6 != nil && (oldV6 == nil || !oldV6.IP.Equal(newIPv6.IP)) {
		oldAddr6 := ""
		if oldV6 != nil {
			oldAddr6 = host128(oldV6.IP)
		}
		if err := tun.ChangeIP(nm.tunName, oldAddr6, host128(newIPv6.IP)); err != nil {
			plog.G(ctx).Warnf("[NetworkManager] Change IPv6 failed: %v", err)
		}
	}

	nm.localTunIPv4 = newIPv4
	if newIPv6 != nil {
		nm.localTunIPv6 = newIPv6
	}

	// Reconcile only the device's own host-route: cluster CIDR routes are
	// device-scoped (added as `dev <tun>`, no source IP) and survive the change,
	// but the /32 (and /128) route for the TUN IP itself must follow the new IP.
	if oldV4 != nil {
		_ = tun.DeleteRoutes(nm.tunName, types.Route{Dst: net.IPNet{IP: oldV4.IP, Mask: net.CIDRMask(32, 32)}})
	}
	if err := tun.AddRoutes(nm.tunName, types.Route{Dst: net.IPNet{IP: newIPv4.IP, Mask: net.CIDRMask(32, 32)}}); err != nil {
		plog.G(ctx).Warnf("[NetworkManager] add host route for %s failed: %v", newIPv4.IP, err)
	}
	if newIPv6 != nil {
		if oldV6 != nil {
			_ = tun.DeleteRoutes(nm.tunName, types.Route{Dst: net.IPNet{IP: oldV6.IP, Mask: net.CIDRMask(128, 128)}})
		}
		_ = tun.AddRoutes(nm.tunName, types.Route{Dst: net.IPNet{IP: newIPv6.IP, Mask: net.CIDRMask(128, 128)}})
	}

	plog.G(ctx).Infof("[NetworkManager] TUN IP changed: v4=%s v6=%s on %s", newIPv4, newIPv6, nm.tunName)
	return nil
}

// StartIPWatcher launches a background goroutine that connects to the xds's
// TunConfigService and watches for IP changes. When a change is detected, it calls ChangeTunIP.
// Uses the OwnerID from NetworkConfig for identification.
func (nm *NetworkManager) StartIPWatcher(ctx context.Context) {
	if nm.cfg.OwnerID == "" {
		return
	}
	go nm.watchTunIPFromXDS(ctx)
}

func (nm *NetworkManager) watchTunIPFromXDS(ctx context.Context) {
	if nm.controlPlaneLocalPort == 0 {
		return
	}
	target := fmt.Sprintf("127.0.0.1:%d", nm.controlPlaneLocalPort)

	var currentVersion int64

	for ctx.Err() == nil {
		err := nm.doWatchTunIP(ctx, target, &currentVersion)
		if err != nil && ctx.Err() == nil {
			plog.G(ctx).Debugf("[IPWatcher] Watch disconnected: %v, retrying in 10s", err)
		}
		select {
		case <-time.After(ipWatcherRetryInterval):
		case <-ctx.Done():
			return
		}
	}
}

func (nm *NetworkManager) doWatchTunIP(ctx context.Context, target string, currentVersion *int64) error {
	conn, err := grpc.DialContext(ctx, target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("dial %s: %w", target, err)
	}
	defer conn.Close()

	client := rpc.NewTunConfigServiceClient(conn)

	// Reconnect self-heal: re-assert our allocation before watching. After a long
	// disconnect the server may have reaped our lease (and won't push anything), so
	// we proactively GetTunIP. ExcludeIPs omits our OWN current TUN IP so the server
	// can hand it back (sticky via lastIPs) while still avoiding sibling/local IPs.
	if resp, gerr := client.GetTunIP(ctx, &rpc.TunIPRequest{
		OwnerID:    nm.cfg.OwnerID,
		Namespace:  nm.cfg.ManagerNamespace,
		ExcludeIPs: nm.excludesForReassert(),
		Hostname:   nm.cfg.Hostname,
	}); gerr == nil {
		v4, v6 := parseTunIPResponse(resp)
		nm.applyPushedTunIP(ctx, client, v4, v6)
		*currentVersion = resp.Version
	} else {
		plog.G(ctx).Debugf("[IPWatcher] self-heal GetTunIP failed: %v", gerr)
	}

	stream, err := client.WatchTunIP(ctx, &rpc.TunIPRequest{
		OwnerID:   nm.cfg.OwnerID,
		Namespace: nm.cfg.ManagerNamespace,
	})
	if err != nil {
		return fmt.Errorf("WatchTunIP: %w", err)
	}

	for {
		resp, err := stream.Recv()
		if err != nil {
			return err
		}
		if resp.Version != *currentVersion && *currentVersion != 0 {
			newV4, newV6 := parseTunIPResponse(resp)
			if resp.DryRun {
				// A proposal: validate locally, then confirm (→ server commits) or decline.
				nm.handleProposal(ctx, client, newV4, newV6)
			} else {
				// A committed change: validate, then apply or reject for a safe IP.
				nm.applyPushedTunIP(ctx, client, newV4, newV6)
			}
		}
		*currentVersion = resp.Version
	}
}

// handleProposal responds to a dry-run proposal: if the candidate is usable it
// confirms (GetTunIP{ConfirmIP}, whose committed response is then applied); if it
// conflicts locally it declines (GetTunIP{ExcludeIPs:[candidate]}), so the server
// drops the proposal without ever committing — no rollback needed.
func (nm *NetworkManager) handleProposal(ctx context.Context, client rpc.TunConfigServiceClient, v4, v6 *net.IPNet) {
	// Each family is validated and confirmed/declined independently, so an operator
	// can edit v4, v6, or both.
	if v4 != nil {
		nm.handleProposalFamily(ctx, client, v4)
	}
	if v6 != nil {
		nm.handleProposalFamily(ctx, client, v6)
	}
}

// handleProposalFamily validates one proposed family candidate and confirms it
// (server then commits and returns the committed alloc to apply) or declines it.
func (nm *NetworkManager) handleProposalFamily(ctx context.Context, client rpc.TunConfigServiceClient, cand *net.IPNet) {
	excludes := buildExcludeIPs(nm.cfg.ReservedTunIPs)
	var pv4, pv6 *net.IPNet
	if cand.IP.To4() == nil {
		pv6 = cand
	} else {
		pv4 = cand
	}
	if tunIPConflicts(pv4, pv6, nm.localTunIPv4, nm.localTunIPv6, excludes) {
		plog.G(ctx).Warnf("[IPWatcher] declining proposed TUN IP %s (conflicts with a local/sibling device)", cand.IP)
		rej := append(append([]string{}, excludes...), cand.IP.String())
		if _, err := client.GetTunIP(ctx, &rpc.TunIPRequest{
			OwnerID:    nm.cfg.OwnerID,
			Namespace:  nm.cfg.ManagerNamespace,
			ExcludeIPs: rej,
			Hostname:   nm.cfg.Hostname,
		}); err != nil {
			plog.G(ctx).Errorf("[IPWatcher] decline proposed IP %s: %v", cand.IP, err)
		}
		return
	}
	// Usable → confirm; the committed response carries the full alloc to apply.
	resp, err := client.GetTunIP(ctx, &rpc.TunIPRequest{
		OwnerID:    nm.cfg.OwnerID,
		Namespace:  nm.cfg.ManagerNamespace,
		ConfirmIP:  cand.IP.String(),
		ExcludeIPs: excludes,
		Hostname:   nm.cfg.Hostname,
	})
	if err != nil {
		plog.G(ctx).Errorf("[IPWatcher] confirm proposed IP %s: %v", cand.IP, err)
		return
	}
	cv4, cv6 := parseTunIPResponse(resp)
	nm.applyPushedTunIP(ctx, client, cv4, cv6)
}

// tunIPConflicts reports whether a server-pushed IP collides with another local
// TUN device or a host interface, in which case the client must not adopt it.
// The owner's OWN current TUN IP is never a conflict (a rollback re-pushes it,
// and treating it as a conflict would cause an endless reject loop).
func tunIPConflicts(v4, v6, ownV4, ownV6 *net.IPNet, excludes []string) bool {
	set := make(map[string]bool, len(excludes))
	for _, e := range excludes {
		set[e] = true
	}
	isOwn := func(ip, own *net.IPNet) bool {
		return ip != nil && own != nil && ip.IP.Equal(own.IP)
	}
	if v4 != nil && set[v4.IP.String()] && !isOwn(v4, ownV4) {
		return true
	}
	if v6 != nil && set[v6.IP.String()] && !isOwn(v6, ownV6) {
		return true
	}
	return false
}

// excludesForReassert is buildExcludeIPs minus this connection's own current TUN
// IP, so a re-assert/self-heal GetTunIP can reclaim the same IP (server prefers
// lastIPs) instead of the allocator skipping it as "excluded".
func (nm *NetworkManager) excludesForReassert() []string {
	own := make(map[string]bool, 2)
	if nm.localTunIPv4 != nil {
		own[nm.localTunIPv4.IP.String()] = true
	}
	if nm.localTunIPv6 != nil {
		own[nm.localTunIPv6.IP.String()] = true
	}
	all := buildExcludeIPs(nm.cfg.ReservedTunIPs)
	out := make([]string, 0, len(all))
	for _, e := range all {
		if !own[e] {
			out = append(out, e)
		}
	}
	return out
}

// applyPushedTunIP adopts a server-pushed TUN IP, or rejects it (asking the server
// for a different one, which rolls a manual change back to the prior IP) when it
// conflicts with a local/sibling device. The client never blindly trusts a push.
func (nm *NetworkManager) applyPushedTunIP(ctx context.Context, client rpc.TunConfigServiceClient, newV4, newV6 *net.IPNet) {
	if newV4 == nil {
		return
	}
	excludes := buildExcludeIPs(nm.cfg.ReservedTunIPs)
	if tunIPConflicts(newV4, newV6, nm.localTunIPv4, nm.localTunIPv6, excludes) {
		plog.G(ctx).Warnf("[IPWatcher] pushed TUN IP %s conflicts with a local/sibling device; rejecting", newV4.IP)
		rej := append(append([]string{}, excludes...), newV4.IP.String())
		if newV6 != nil {
			rej = append(rej, newV6.IP.String())
		}
		if _, err := client.GetTunIP(ctx, &rpc.TunIPRequest{
			OwnerID:    nm.cfg.OwnerID,
			Namespace:  nm.cfg.ManagerNamespace,
			ExcludeIPs: rej,
			Hostname:   nm.cfg.Hostname,
		}); err != nil {
			plog.G(ctx).Errorf("[IPWatcher] reject pushed IP %s: %v", newV4.IP, err)
		}
		return
	}
	// No-op only when BOTH families already match — otherwise a v6-only change (v4
	// unchanged) would be wrongly skipped.
	v4Same := nm.localTunIPv4 != nil && nm.localTunIPv4.IP.Equal(newV4.IP)
	v6Same := newV6 == nil || (nm.localTunIPv6 != nil && nm.localTunIPv6.IP.Equal(newV6.IP))
	if v4Same && v6Same {
		return
	}
	if err := nm.ChangeTunIP(ctx, newV4, newV6); err != nil {
		plog.G(ctx).Errorf("[IPWatcher] ChangeTunIP failed: %v", err)
	}
}

func parseTunIPResponse(resp *rpc.TunIPResponse) (ipv4, ipv6 *net.IPNet) {
	if resp.IPv4 != "" {
		ip, cidr, err := net.ParseCIDR(resp.IPv4)
		if err == nil {
			ipv4 = &net.IPNet{IP: ip, Mask: cidr.Mask}
		}
	}
	if resp.IPv6 != "" {
		ip, cidr, err := net.ParseCIDR(resp.IPv6)
		if err == nil {
			ipv6 = &net.IPNet{IP: ip, Mask: cidr.Mask}
		}
	}
	return
}

// nextPortForwardDelay computes the next reconnect backoff. A session that lasted at
// least portForwardHealthySession before dropping resets to the fast initial delay;
// otherwise (a short-lived failure, e.g. an apiserver stall) the delay doubles, capped
// at portForwardReconnectMaxDelay. Pure function for testability.
func nextPortForwardDelay(cur, sessionDuration time.Duration) time.Duration {
	if sessionDuration >= portForwardHealthySession {
		return portForwardReconnectDelay
	}
	next := cur * 2
	if next < portForwardReconnectDelay {
		next = portForwardReconnectDelay
	}
	if next > portForwardReconnectMaxDelay {
		next = portForwardReconnectMaxDelay
	}
	return next
}

// portForward sets up port-forwarding to the traffic manager pod with automatic
// retry when the pod is recreated or the connection drops.
func (nm *NetworkManager) portForward(ctx context.Context, portPair []string) error {
	firstCtx, firstCancelFunc := context.WithCancel(ctx)
	defer firstCancelFunc()
	errChan := make(chan error, 1)
	go func() {
		first := true
		delay := portForwardReconnectDelay
		for ctx.Err() == nil {
			sessionStart := time.Now()
			err := nm.portForwardOnce(ctx, portPair, first, firstCancelFunc)
			sessionDuration := time.Since(sessionStart)
			if first {
				if err != nil {
					netutil.SafeWrite(errChan, err)
					return
				}
			} else {
				plog.G(ctx).Infof("[Perf] Port-forward session ended after %v, reconnecting in %v...", sessionDuration, delay)
			}
			first = false
			// Exponential backoff on consecutive short-lived sessions so a stalled
			// apiserver is not hammered with a Pods.List + TLS handshake every 200ms
			// (which prolongs the stall). A healthy session that then drops resets to
			// the fast initial delay so normal pod recreation still reconnects quickly.
			delay = nextPortForwardDelay(delay, sessionDuration)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return
			}
		}
	}()
	ticker := time.NewTicker(portForwardStartTimeout)
	defer ticker.Stop()
	select {
	case <-ticker.C:
		return config.ErrPortForwardTimeout
	case err := <-errChan:
		return err
	case <-firstCtx.Done():
		return nil
	}
}

// portForwardOnce runs a single port-forward session to the traffic manager pod.
func (nm *NetworkManager) portForwardOnce(ctx context.Context, portPair []string, first bool, onReady func()) error {
	ctx2, cancelFunc2 := context.WithTimeout(ctx, portForwardPodListTimeout)
	defer cancelFunc2()
	podList, err := nm.cfg.GetRunningPodList(ctx2)
	if err != nil {
		plog.G(ctx).Debugf("Failed to get running pod: %v", err)
		return err
	}
	pod := podList[0]
	// add route in case the pod was recreated with a new IP that is not yet routable
	_ = nm.AddRoute(util.GetPodIP(pod)...)

	childCtx, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()

	readyChan := make(chan struct{})
	podName := pod.GetName()
	// detect pod deletion so we can redo port-forward
	go util.CheckPodStatus(childCtx, cancelFunc, podName, nm.cfg.Clientset.CoreV1().Pods(nm.cfg.ManagerNamespace))
	// The data-plane heartbeat watchdog is the sole liveness-based reconnect trigger.
	go nm.watchDataPlaneLiveness(childCtx, cancelFunc, readyChan)
	if first {
		go func() {
			select {
			case <-readyChan:
				onReady()
			case <-childCtx.Done():
			}
		}()
	}

	pfStart := time.Now()
	err = util.PortForwardPod(
		nm.cfg.Config,
		nm.cfg.RESTClient,
		podName,
		nm.cfg.ManagerNamespace,
		portPair,
		readyChan,
		childCtx.Done(),
		nil,
		plog.G(ctx).Logger.Out,
	)
	plog.G(ctx).Infof("[Perf] PortForwardPod for %s exited after %v, err=%v", podName, time.Since(pfStart), err)
	return nil
}

// watchDataPlaneLiveness force-reconnects the current port-forward session when the data plane goes
// silent while the session still looks alive. A k8s port-forward can "black-hole": the local TCP
// connects and the session appears up, but no data traverses (seen against flaky cluster network
// paths / SPDY proxies). The xDS health check catches most such cases, but it probes the
// control-plane port; a data-only stall (only the TCP/UDP forward stream wedges) would slip past it.
// HeartbeatStats records ICMP echo replies from the server gateway — a direct end-to-end data-plane
// liveness signal — so a session that never produces a fresh reply (startupDeadline) or falls silent
// after having produced one (steadyThreshold) is black-holed. cancelFunc tears the session down; the
// portForward loop then reconnects. heartbeatStats is allocated in newNetworkManager (never nil).
// This runs for every session, including the first (typically longest-lived) one. It is the sole
// data-plane reconnect trigger — the xDS health check only reports control-plane status.
func (nm *NetworkManager) watchDataPlaneLiveness(ctx context.Context, cancelFunc context.CancelFunc, readyChan <-chan struct{}) {
	sessionStart := time.Now()
	// A session that never even becomes ready is still black-holed: bound the wait so it reconnects.
	select {
	case <-readyChan:
	case <-time.After(livenessStartupDeadline):
		plog.G(ctx).Warnf("Port-forward not ready within %v; reconnecting", livenessStartupDeadline)
		cancelFunc()
		return
	case <-ctx.Done():
		return
	}
	watchLiveness(ctx, cancelFunc, sessionStart, livenessCheckInterval, livenessStartupDeadline, livenessSteadyThreshold, nm.heartbeatStats.LastReply)
}

// watchLiveness force-reconnects (cancelFunc) a black-holed session using the data-plane heartbeat
// echo reply as the liveness signal. A session is "primed" once it produces a reply newer than
// sessionStart — proving fresh data flowed on THIS session (lastReply is shared across sessions, so
// an older timestamp does not count). Two independent deadlines, both measured so a healthy session
// is never torn down prematurely:
//   - not yet primed: reconnect if the session has run startupDeadline without any fresh reply
//     (never came up, or black-holed from the start). startupDeadline must exceed the time a healthy
//     reconnected session needs to prime (slot reconnect + RTT).
//   - primed: reconnect if the last fresh reply is now older than steadyThreshold (went silent).
//
// Extracted so it can be driven with small timings and a fake reply source in tests.
func watchLiveness(ctx context.Context, cancelFunc context.CancelFunc, sessionStart time.Time, interval, startupDeadline, steadyThreshold time.Duration, lastReply func() time.Time) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	primed := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			last := lastReply()
			if !primed {
				if last.After(sessionStart) {
					primed = true
					continue
				}
				if time.Since(sessionStart) >= startupDeadline {
					plog.G(ctx).Warnf("Data plane never came up (no heartbeat echo reply within %v); reconnecting port-forward", startupDeadline)
					cancelFunc()
					return
				}
				continue
			}
			if time.Since(last) >= steadyThreshold {
				plog.G(ctx).Warnf("Data plane went silent (no heartbeat echo reply for %v); reconnecting port-forward", steadyThreshold)
				cancelFunc()
				return
			}
		}
	}
}

// startTUN allocates a TUN IP and creates the local TUN device with a gvisor network stack.
func (nm *NetworkManager) startTUN(ctx context.Context, forwardAddress string) error {
	tlsSecret, err := nm.cfg.Clientset.CoreV1().Secrets(nm.cfg.ManagerNamespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return err
	}

	forwardNode, err := core.ParseNode(forwardAddress)
	if err != nil {
		plog.G(ctx).Errorf("Failed to parse forward node %s: %v", forwardAddress, err)
		return err
	}

	var cidrList []*net.IPNet
	cidrList = append(cidrList, nm.cfg.CIDRs...)
	for _, s := range nm.cfg.ExtraRouteInfo.ExtraCIDR {
		var ipNet *net.IPNet
		_, ipNet, err = net.ParseCIDR(s)
		if err != nil {
			return fmt.Errorf("invalid extra-cidr %s: %w", s, err)
		}
		cidrList = append(cidrList, ipNet)
	}

	// Build CIDR routes first; host routes for the TUN IP are appended after allocation.
	var routes []types.Route
	for _, ipNet := range nm.dedupAndFilterCIDRs(cidrList) {
		if ipNet != nil && !ipNet.IP.IsLoopback() {
			routes = append(routes, types.Route{Dst: *ipNet})
		}
	}

	if err := nm.ensureTunIPAllocated(ctx); err != nil {
		return err
	}

	// Append /32 and /128 host routes for the allocated TUN IPs after CIDR routes.
	routes = append(routes, tunHostRoutes(nm.localTunIPv4, nm.localTunIPv6)...)

	tunConfig := buildTunConfig(nm.localTunIPv4, nm.localTunIPv6, routes)

	return nm.startTunServer(ctx, forwardNode, forwardAddress, tlsSecret.Data, tunConfig)
}

// ensureTunIPAllocated allocates the TUN IP via rentIP if not already allocated.
// It emits StepStart/StepDone progress messages and logs the allocated IPs.
func (nm *NetworkManager) ensureTunIPAllocated(ctx context.Context) error {
	if nm.localTunIPv4 == nil {
		plog.StepStart(ctx, "Allocating TUN IP")
		if err := nm.rentIP(ctx); err != nil {
			return err
		}
		ipSummary := nm.localTunIPv4.IP.String()
		if nm.localTunIPv6 != nil {
			ipSummary += " / " + nm.localTunIPv6.IP.String()
		}
		plog.StepDone(ctx, "Allocated TUN IP %s", ipSummary)
	}
	v6Str := "<none>"
	if nm.localTunIPv6 != nil {
		v6Str = nm.localTunIPv6.IP.String()
	}
	plog.G(ctx).Debugf("IPv4: %s, IPv6: %s", nm.localTunIPv4.IP.String(), v6Str)
	return nil
}

// tunHostRoutes builds the /32 (IPv4) and /128 (IPv6) host routes for the TUN device's
// own IPs. Either argument may be nil (family absent → no route for that family).
func tunHostRoutes(v4, v6 *net.IPNet) []types.Route {
	var routes []types.Route
	if v4 != nil {
		routes = append(routes, types.Route{Dst: net.IPNet{IP: v4.IP, Mask: net.CIDRMask(32, 32)}})
	}
	if v6 != nil {
		routes = append(routes, types.Route{Dst: net.IPNet{IP: v6.IP, Mask: net.CIDRMask(128, 128)}})
	}
	return routes
}

// buildTunConfig assembles a tun.Config from the allocated TUN IPs and the pre-built
// route list. IPv6 address is only set when IPv6 is enabled on the host and v6 is non-nil.
func buildTunConfig(v4, v6 *net.IPNet, routes []types.Route) tun.Config {
	cfg := tun.Config{
		Addr:   (&net.IPNet{IP: v4.IP, Mask: net.CIDRMask(32, 32)}).String(),
		Routes: routes,
		MTU:    config.DefaultMTU,
	}
	if enable, _ := netutil.IsIPv6Enabled(); enable && v6 != nil {
		cfg.Addr6 = (&net.IPNet{IP: v6.IP, Mask: net.CIDRMask(128, 128)}).String()
	}
	return cfg
}

// startTunServer creates the TUN listener, wires up the gvisor handler and forwarder,
// starts the server goroutine, and resolves the TUN device name.
func (nm *NetworkManager) startTunServer(ctx context.Context, forwardNode *core.Node, forwardAddress string, secretData map[string][]byte, tunConfig tun.Config) error {
	forwarder := &core.Forwarder{
		Addr:        forwardNode.Addr,
		Connector:   core.NewUDPOverTCPConnector(),
		Transporter: core.TCPTransporter(secretData),
		MaxRetries:  5,
	}

	plog.StepStart(ctx, "Creating TUN device")
	// heartbeatStats is allocated once in newNetworkManager; reuse it (do not re-assign) so the
	// liveness watchdog and status queries observe the same instance without a data race.
	handler := core.TunHandler(forwarder, core.NewRouteHub(), nm.heartbeatStats)
	listener, err := tun.Listener(tunConfig)
	if err != nil {
		plog.G(ctx).Errorf("Failed to create tun listener: %v", err)
		return fmt.Errorf("create tun listener: %w: %w", err, config.ErrTunDeviceFailed)
	}

	server := core.Server{
		Listener: listener,
		Handler:  handler,
	}

	go func() {
		if err := Run(ctx, []core.Server{server}); err != nil && ctx.Err() == nil {
			plog.G(ctx).Errorf("[Client] Local TUN server exited: %v", err)
		}
	}()
	plog.G(ctx).Debugf("[Client] TUN server started, forwarding to %s", forwardAddress)

	nm.tunName, err = nm.getTunDeviceName()
	if err != nil {
		return err
	}
	plog.StepDone(ctx, "Created TUN device %q", nm.tunName)
	return nil
}

// getTunDeviceName resolves the TUN device name from the configured IPs.
func (nm *NetworkManager) getTunDeviceName() (string, error) {
	var ips []net.IP
	if nm.localTunIPv4 != nil {
		ips = append(ips, nm.localTunIPv4.IP)
	}
	if nm.localTunIPv6 != nil {
		ips = append(ips, nm.localTunIPv6.IP)
	}
	device, err := netutil.GetTunDevice(ips...)
	if err != nil {
		return "", err
	}
	return device.Name, nil
}

// setupDNS configures DNS resolution for the cluster.
func (nm *NetworkManager) setupDNS(ctx context.Context) error {
	podList, err := nm.cfg.GetRunningPodList(ctx)
	if err != nil {
		plog.G(ctx).Errorf("Get running pod list failed, err: %v", err)
		return err
	}
	pod := podList[0]
	plog.StepStart(ctx, "Configuring DNS")
	plog.G(ctx).Debugf("Getting DNS service IP from pod...")
	relovConf, err := util.GetDNSServiceIPFromPod(ctx, nm.cfg.Clientset, nm.cfg.Config, pod.GetName(), nm.cfg.ManagerNamespace)
	if err != nil {
		plog.G(ctx).Errorln(err)
		return err
	}

	marshal, _ := json.Marshal(relovConf)
	plog.G(ctx).Debugf("Get DNS service config: %v", string(marshal))
	var svc *v1.Service
	svc, err = nm.cfg.Clientset.CoreV1().Services(nm.cfg.ManagerNamespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return err
	}
	err = detectNameserver(ctx, relovConf, svc.Spec.ClusterIP, pod.Status.PodIP)
	if err != nil {
		return err
	}

	plog.G(ctx).Debugf("Adding extra domains to hosts...")
	if err = nm.AddExtraRoute(ctx, relovConf.Servers); err != nil {
		plog.G(ctx).Errorf("Add extra route failed: %v", err)
		return err
	}

	ns := nm.listResolvableNamespaces(ctx)

	plog.G(ctx).Debugf("Listing namespace %s services...", nm.cfg.WorkloadNamespace)
	nm.dnsConfig = &dns.Config{
		Config:   relovConf,
		Ns:       ns,
		Services: []v1.Service{},
		TunName:  nm.tunName,
		Hosts:    nm.extraHost,
		Lock:     nm.cfg.Lock,
		HowToGetExternalName: func(domain string) (string, error) {
			ip, err := resolveDomainViaClusterDNS(ctx, relovConf.Servers, domain)
			if err != nil {
				return "", err
			}
			return ip.String(), nil
		},
	}
	plog.G(ctx).Debugf("Setting up DNS resolver on device %s...", nm.tunName)
	if err = nm.dnsConfig.SetupDNS(ctx); err != nil {
		return err
	}
	if len(relovConf.Servers) > 0 {
		plog.StepDone(ctx, "Configured DNS (cluster DNS %s)", relovConf.Servers[0])
	} else {
		plog.StepDone(ctx, "Configured DNS")
	}

	plog.StepStart(ctx, "Writing service records to the hosts file")
	// dump service in current namespace for support DNS resolve service:port
	n, err := nm.dnsConfig.AddServiceNameToHosts(ctx, nm.extraHost...)
	if err != nil {
		return err
	}
	plog.StepDone(ctx, "Wrote %d service records to the hosts file (namespace %q)", n, nm.cfg.WorkloadNamespace)
	return nil
}

// listResolvableNamespaces returns the full list of namespaces to use for DNS resolution.
// WorkloadNamespace is always first. All other namespaces are appended in list order,
// skipping any duplicate. When the List call fails, only WorkloadNamespace is returned.
func (nm *NetworkManager) listResolvableNamespaces(ctx context.Context) []string {
	ns := []string{nm.cfg.WorkloadNamespace}
	list, err := nm.cfg.Clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: 500})
	if err == nil {
		for _, item := range list.Items {
			if !sets.New[string](ns...).Has(item.Name) {
				ns = append(ns, item.Name)
			}
		}
	}
	return ns
}

// dedupAndFilterCIDRs removes overlapping CIDRs and filters out those containing API server IPs.
func (nm *NetworkManager) dedupAndFilterCIDRs(cidrs []*net.IPNet) []*net.IPNet {
	return util.RemoveCIDRsContainingIPs(util.RemoveLargerOverlappingCIDRs(cidrs), nm.cfg.APIServerIPs)
}

// AddRoute adds IP addresses to the system route table via the TUN device,
// skipping any that match the API server or are already routed through the TUN.
func (nm *NetworkManager) AddRoute(ipStrList ...string) error {
	if nm.tunName == "" {
		return nil
	}
	var routes []types.Route
	r, _ := netroute.New()
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
		if r != nil {
			ifi, _, _, err := r.Route(ip)
			if err == nil && ifi.Name == nm.tunName {
				continue
			}
		}
		routes = append(routes, types.Route{Dst: net.IPNet{IP: ip, Mask: mask}})
	}
	if len(routes) == 0 {
		return nil
	}
	return tun.AddRoutes(nm.tunName, routes...)
}

// addTrafficManagerServiceRoute adds /32 TUN routes for the traffic manager's
// ClusterIPs. Called synchronously during Start, before the async route watcher,
// so that dialManager's SYN travels through TUN from the start — preventing
// a mid-handshake route split (SYN via eth0, ACK via TUN) that makes gvisor
// RST the connection it never saw a SYN for.
func (nm *NetworkManager) addTrafficManagerServiceRoute(ctx context.Context) error {
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

// AddRouteCIDR adds CIDR prefixes (e.g. server-pushed aggregated pod prefixes like
// "10.244.3.0/24") to the route table via the TUN device, skipping any that are
// invalid or already routed through the TUN. Unlike AddRoute (which builds /32 /128
// host routes from single IPs), this routes whole prefixes, matching how the cluster
// CIDRs are installed at connect.
func (nm *NetworkManager) AddRouteCIDR(cidrs ...string) error {
	if nm.tunName == "" {
		return nil
	}
	var routes []types.Route
	r, _ := netroute.New()
	for _, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil || ipNet == nil || ipNet.IP.IsLoopback() {
			continue
		}
		if r != nil {
			if ifi, _, _, rerr := r.Route(ipNet.IP); rerr == nil && ifi.Name == nm.tunName {
				continue
			}
		}
		routes = append(routes, types.Route{Dst: *ipNet})
	}
	if len(routes) == 0 {
		return nil
	}
	return tun.AddRoutes(nm.tunName, routes...)
}

// StartRouteWatcher subscribes to the traffic manager's WatchNamespaceRoutes stream
// for the workload namespace and applies pushed pod route prefixes (to the route
// table) and service records (to DNS), replacing the former client-side cluster-wide
// pod/service informers. It runs in the background under ctx; on any failure it
// degrades to CIDR-only routing (the cluster CIDRs are already routed).
func (nm *NetworkManager) StartRouteWatcher(ctx context.Context) {
	if nm.controlPlaneLocalPort == 0 {
		return
	}
	go nm.watchNamespaceRoutes(ctx)
}

func (nm *NetworkManager) watchNamespaceRoutes(ctx context.Context) {
	target := fmt.Sprintf("127.0.0.1:%d", nm.controlPlaneLocalPort)
	var version int64
	for ctx.Err() == nil {
		err := nm.doWatchNamespaceRoutes(ctx, target, &version)
		if err != nil && ctx.Err() == nil {
			plog.G(ctx).Debugf("[RouteWatcher] disconnected: %v, retrying in %s", err, ipWatcherRetryInterval)
		}
		select {
		case <-time.After(ipWatcherRetryInterval):
		case <-ctx.Done():
			return
		}
	}
}

func (nm *NetworkManager) doWatchNamespaceRoutes(ctx context.Context, target string, version *int64) error {
	conn, err := grpc.DialContext(ctx, target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.UseCompressor(gzip.Name)),
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("dial %s: %w", target, err)
	}
	defer conn.Close()

	client := rpc.NewTunConfigServiceClient(conn)
	stream, err := client.WatchNamespaceRoutes(ctx, &rpc.NamespaceRoutesRequest{Namespace: nm.cfg.WorkloadNamespace})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			// Old traffic manager without server-side discovery: nothing to do, the
			// cluster CIDR routes already cover routing. Do not retry-spin.
			plog.G(ctx).Infof("[RouteWatcher] manager has no WatchNamespaceRoutes; using CIDR-only routing")
			<-ctx.Done()
			return nil
		}
		return fmt.Errorf("WatchNamespaceRoutes: %w", err)
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
			// Manager lacks RBAC to watch this namespace. Degrade to CIDR-only routing.
			plog.G(ctx).Warnf("[RouteWatcher] server route discovery disabled for namespace %q (no RBAC); using CIDR-only routing", nm.cfg.WorkloadNamespace)
			<-ctx.Done()
			return nil
		}
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
				if nm.dnsConfig != nil {
					nm.dnsConfig.UpdateServices(ctx, svcs)
				}
			},
		)
		*version = resp.Version
	}
}

// applyRouteFrame applies one WatchNamespaceRoutes frame to the running state: it resets
// the service set on a snapshot, adds pushed pod CIDR routes (via addCIDR), routes the
// upserted services' ClusterIPs (via addServiceIPs), upserts/removes service records, and
// pushes the updated service set to DNS (via setDNS) when it changed.
// Routes are add-only: RemovedPodCIDRs / RemovedServiceKeys are NOT unrouted (a stale route
// pointed at the TUN is harmless), they only affect state. Pure of gRPC/NetworkManager for
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
func (nm *NetworkManager) AddExtraRoute(ctx context.Context, servers []string) error {
	if len(nm.cfg.ExtraRouteInfo.ExtraDomain) == 0 {
		return nil
	}

	for _, domain := range nm.cfg.ExtraRouteInfo.ExtraDomain {
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
		nm.extraHost = append(nm.extraHost, dns.Entry{IP: net.ParseIP(ip).String(), Domain: domain})
	}
	return nil
}

// AddExtraNodeIP adds cluster node IPs to the extra CIDR list so they
// get routed through the TUN device.
func (nm *NetworkManager) AddExtraNodeIP(ctx context.Context) error {
	if !nm.cfg.ExtraRouteInfo.ExtraNodeIP {
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
				nm.cfg.ExtraRouteInfo.ExtraCIDR = append(nm.cfg.ExtraRouteInfo.ExtraCIDR, (&net.IPNet{
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
