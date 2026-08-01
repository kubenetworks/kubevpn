package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/libp2p/go-netroute"
	v1 "k8s.io/api/core/v1"
	apinetworkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	informerv1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	v2 "k8s.io/client-go/kubernetes/typed/networking/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
	"github.com/wencaiwulue/kubevpn/v2/pkg/driver"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
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
	// portForwardReconnectDelay is the pause between port-forward reconnect attempts.
	portForwardReconnectDelay = 200 * time.Millisecond
	// routeWatchInterval is the debounce/refresh period for the dynamic route-table watcher.
	routeWatchInterval = 15 * time.Second
	// portForwardStartTimeout is how long to wait for the first port-forward session to become ready.
	portForwardStartTimeout = 60 * time.Second
)

// newNetworkManager creates a NetworkManager with the given configuration.
func newNetworkManager(cfg NetworkConfig) *NetworkManager {
	return &NetworkManager{cfg: cfg}
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
// 2. Port-forward to traffic manager (gvisor TCP/UDP + control-plane)
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
		fmt.Sprintf("%d:%d", controlPlanePort, config.PortControlPlane),
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
		return err
	}

	plog.StepStart(nm.ctx, "Adding routes")
	svcInformer, _, err := nm.AddRouteDynamic(nm.ctx)
	if err != nil {
		return err
	}
	plog.StepDone(nm.ctx, "Added %d pod/service routes", len(nm.cfg.CIDRs))

	if err := nm.setupDNS(nm.ctx, svcInformer); err != nil {
		return err
	}

	return nil
}

// rentIP allocates a TUN IP from the control-plane's TunConfigService via the
// already-established port-forward. Passes local interface IPs as ExcludeIPs
// so the server avoids conflicts. Retries on the rare race where a new
// interface appears between collecting addresses and receiving the allocation.
func (nm *NetworkManager) rentIP(ctx context.Context) error {
	target := fmt.Sprintf("127.0.0.1:%d", nm.controlPlaneLocalPort)
	conn, err := grpc.DialContext(ctx, target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("dial control-plane for RentIP: %w", err)
	}
	defer conn.Close()

	client := rpc.NewTunConfigServiceClient(conn)

	const maxRetries = 15
	for i := 0; i < maxRetries; i++ {
		resp, err := client.GetTunIP(ctx, &rpc.TunIPRequest{
			OwnerID:    nm.cfg.OwnerID,
			Namespace:  nm.cfg.ManagerNamespace,
			ExcludeIPs: buildExcludeIPs(nm.cfg.ReservedTunIPs),
			Hostname:   nm.cfg.Hostname,
		})
		if err != nil {
			return fmt.Errorf("get TUN IP from control-plane: %w", err)
		}

		ip4, cidr4, err := net.ParseCIDR(resp.IPv4)
		if err != nil || cidr4 == nil {
			return fmt.Errorf("invalid IPv4 from control-plane: %q", resp.IPv4)
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

	return fmt.Errorf("failed to allocate a non-conflicting TUN IP after %d attempts", maxRetries)
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
		return fmt.Errorf("TUN device not started")
	}
	if newIPv4 == nil {
		return fmt.Errorf("new IPv4 is nil")
	}

	oldV4, oldV6 := nm.localTunIPv4, nm.localTunIPv6
	// Always operate on host masks (/32, /128) — matching how startTUN created the
	// device. nm.localTunIPv4 carries the pool mask (/16); passing that as oldAddr
	// makes the delete miss the /32 actually on the device, and adds the new address
	// with a wrong /16 mask (leaving the old IP behind). Only touch a family that
	// actually changed.
	host32 := func(ip net.IP) string { return (&net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)}).String() }
	host128 := func(ip net.IP) string { return (&net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}).String() }

	if oldV4 == nil || !oldV4.IP.Equal(newIPv4.IP) {
		oldAddr := ""
		if oldV4 != nil {
			oldAddr = host32(oldV4.IP)
		}
		if err := tun.ChangeIP(nm.tunName, oldAddr, host32(newIPv4.IP)); err != nil {
			return fmt.Errorf("change IPv4 on %s: %w", nm.tunName, err)
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

// StartIPWatcher launches a background goroutine that connects to the control-plane's
// TunConfigService and watches for IP changes. When a change is detected, it calls ChangeTunIP.
// Uses the OwnerID from NetworkConfig for identification.
func (nm *NetworkManager) StartIPWatcher(ctx context.Context) {
	if nm.cfg.OwnerID == "" {
		return
	}
	go nm.watchTunIPFromControlPlane(ctx)
}

func (nm *NetworkManager) watchTunIPFromControlPlane(ctx context.Context) {
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

// portForward sets up port-forwarding to the traffic manager pod with automatic
// retry when the pod is recreated or the connection drops.
func (nm *NetworkManager) portForward(ctx context.Context, portPair []string) error {
	firstCtx, firstCancelFunc := context.WithCancel(ctx)
	defer firstCancelFunc()
	errChan := make(chan error, 1)
	go func() {
		first := true
		for ctx.Err() == nil {
			sessionStart := time.Now()
			err := nm.portForwardOnce(ctx, portPair, first, firstCancelFunc)
			sessionDuration := time.Since(sessionStart)
			if first {
				if err != nil {
					util.SafeWrite(errChan, err)
					return
				}
			} else {
				plog.G(ctx).Infof("[Perf] Port-forward session ended after %v, reconnecting...", sessionDuration)
			}
			first = false
			time.Sleep(portForwardReconnectDelay)
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
	controlPlanePort, _, _ := strings.Cut(portPair[2], ":")
	go healthCheckGRPC(childCtx, cancelFunc, readyChan, controlPlanePort)
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

	var routes []types.Route
	for _, ipNet := range nm.dedupAndFilterCIDRs(cidrList) {
		if ipNet != nil && !ipNet.IP.IsLoopback() {
			routes = append(routes, types.Route{Dst: *ipNet})
		}
	}

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

	if nm.localTunIPv4 != nil {
		routes = append(routes, types.Route{Dst: net.IPNet{IP: nm.localTunIPv4.IP, Mask: net.CIDRMask(32, 32)}})
	}
	if nm.localTunIPv6 != nil {
		routes = append(routes, types.Route{Dst: net.IPNet{IP: nm.localTunIPv6.IP, Mask: net.CIDRMask(128, 128)}})
	}

	tunConfig := tun.Config{
		Addr:   (&net.IPNet{IP: nm.localTunIPv4.IP, Mask: net.CIDRMask(32, 32)}).String(),
		Routes: routes,
		MTU:    config.DefaultMTU,
	}
	if enable, _ := util.IsIPv6Enabled(); enable && nm.localTunIPv6 != nil {
		tunConfig.Addr6 = (&net.IPNet{IP: nm.localTunIPv6.IP, Mask: net.CIDRMask(128, 128)}).String()
	}

	forwarder := &core.Forwarder{
		Addr:        forwardNode.Addr,
		Connector:   core.NewUDPOverTCPConnector(),
		Transporter: core.TCPTransporter(tlsSecret.Data),
		MaxRetries:  5,
	}

	plog.StepStart(ctx, "Creating TUN device")
	nm.heartbeatStats = &core.HeartbeatStats{}
	handler := core.TunHandler(forwarder, core.NewRouteHub(), nm.heartbeatStats)
	listener, err := tun.Listener(tunConfig)
	if err != nil {
		plog.G(ctx).Errorf("Failed to create tun listener: %v", err)
		return err
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
	device, err := util.GetTunDevice(ips...)
	if err != nil {
		return "", err
	}
	return device.Name, nil
}

// setupDNS configures DNS resolution for the cluster.
func (nm *NetworkManager) setupDNS(ctx context.Context, svcInformer cache.SharedIndexInformer) error {
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
	if err = nm.AddExtraRoute(ctx, pod.GetName()); err != nil {
		plog.G(ctx).Errorf("Add extra route failed: %v", err)
		return err
	}

	ns := []string{nm.cfg.WorkloadNamespace}
	list, err := nm.cfg.Clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: 500})
	if err == nil {
		for _, item := range list.Items {
			if !sets.New[string](ns...).Has(item.Name) {
				ns = append(ns, item.Name)
			}
		}
	}

	plog.G(ctx).Debugf("Listing namespace %s services...", nm.cfg.WorkloadNamespace)
	nm.dnsConfig = &dns.Config{
		Config:      relovConf,
		Ns:          ns,
		Services:    []v1.Service{},
		SvcInformer: svcInformer,
		TunName:     nm.tunName,
		Hosts:       nm.extraHost,
		Lock:        nm.cfg.Lock,
		HowToGetExternalName: func(domain string) (string, error) {
			podList, err := nm.cfg.GetRunningPodList(ctx)
			if err != nil {
				return "", err
			}
			pod := podList[0]
			return util.Shell(
				ctx,
				nm.cfg.Clientset,
				nm.cfg.Config,
				pod.GetName(),
				config.ContainerSidecarVPN,
				nm.cfg.ManagerNamespace,
				[]string{"dig", "+short", domain},
			)
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

// AddRouteDynamic starts informers that watch pods and services, adding their
// IPs to the route table as they appear.
func (nm *NetworkManager) AddRouteDynamic(ctx context.Context) (cache.SharedIndexInformer, cache.SharedIndexInformer, error) {
	podNs, svcNs, err := util.GetNsForListPodAndSvc(ctx, nm.cfg.Clientset, []string{v1.NamespaceAll, nm.cfg.WorkloadNamespace})
	if err != nil {
		return nil, nil, err
	}

	conf := rest.CopyConfig(nm.cfg.Config)
	conf.QPS = 1
	conf.Burst = 2
	clientSet, err := kubernetes.NewForConfig(conf)
	if err != nil {
		plog.G(ctx).Errorf("Failed to create clientset: %v", err)
		return nil, nil, err
	}
	indexers := cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}
	svcInformer := informerv1.NewServiceInformer(clientSet, svcNs, 0, indexers)
	if err = nm.watchAndRoute(ctx, svcInformer, func(obj any) []string {
		svc, ok := obj.(*v1.Service)
		if !ok {
			return nil
		}
		return append([]string{svc.Spec.ClusterIP}, svc.Spec.ClusterIPs...)
	}); err != nil {
		return nil, nil, err
	}

	podInformer := informerv1.NewPodInformer(clientSet, podNs, 0, indexers)
	if err = nm.watchAndRoute(ctx, podInformer, func(obj any) []string {
		p, ok := obj.(*v1.Pod)
		if !ok || p.Spec.HostNetwork {
			return nil
		}
		return util.GetPodIP(*p)
	}); err != nil {
		return nil, nil, err
	}

	return svcInformer, podInformer, nil
}

// watchAndRoute starts an informer and a goroutine that periodically extracts
// IPs from the cache and adds them to the route table.
func (nm *NetworkManager) watchAndRoute(ctx context.Context, informer cache.SharedIndexInformer, extractIPs func(any) []string) error {
	ticker := time.NewTicker(routeWatchInterval)
	_, err := informer.AddEventHandler(newTickerResetHandler(ticker))
	if err != nil {
		return err
	}
	go informer.Run(ctx.Done())
	go func() {
		defer ticker.Stop()
		for ; ctx.Err() == nil; <-ticker.C {
			ticker.Reset(routeWatchInterval)
			ips := sets.New[string]()
			for _, obj := range informer.GetIndexer().List() {
				ips.Insert(extractIPs(obj)...)
			}
			if ctx.Err() != nil {
				return
			}
			if ips.Len() == 0 {
				continue
			}
			if err := nm.AddRoute(ips.UnsortedList()...); err != nil {
				plog.G(ctx).Debugf("Add IP to route table failed: %v", err)
			}
		}
	}()
	return nil
}

// AddExtraRoute resolves extra domain names via dig on the traffic manager pod
// and adds their IPs to the route table.
func (nm *NetworkManager) AddExtraRoute(ctx context.Context, name string) error {
	if len(nm.cfg.ExtraRouteInfo.ExtraDomain) == 0 {
		return nil
	}

	// parse cname
	//dig +short db-name.postgres.database.azure.com
	//1234567.privatelink.db-name.postgres.database.azure.com.
	//10.0.100.1
	var parseIP = func(cmdDigOutput string) net.IP {
		for _, s := range strings.Split(cmdDigOutput, "\n") {
			ip := net.ParseIP(strings.TrimSpace(s))
			if ip != nil {
				return ip
			}
		}
		return nil
	}

	// 1) use dig +short query, if ok, just return
	for _, domain := range nm.cfg.ExtraRouteInfo.ExtraDomain {
		output, err := util.Shell(ctx, nm.cfg.Clientset, nm.cfg.Config, name, config.ContainerSidecarVPN, nm.cfg.ManagerNamespace, []string{"dig", "+short", domain})
		if err != nil {
			return fmt.Errorf("failed to resolve DNS for domain by command dig: %w", err)
		}
		var ip string
		if parseIP(output) == nil {
			// try to get ingress record
			ip = getIngressRecord(ctx, nm.cfg.Clientset.NetworkingV1(), []string{v1.NamespaceAll, nm.cfg.ManagerNamespace}, domain)
		} else {
			ip = parseIP(output).String()
		}
		if net.ParseIP(ip) == nil {
			return fmt.Errorf("failed to resolve DNS for domain %s by command dig, output: %s", domain, output)
		}
		err = nm.AddRoute(ip)
		if err != nil {
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
