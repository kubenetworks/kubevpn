package network

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"time"

	"github.com/containernetworking/cni/pkg/types"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
)

type tunAddresses struct {
	localTunIPv4 *net.IPNet
	localTunIPv6 *net.IPNet
}

// LocalTunIPv4 returns the IPv4 TUN address.
func (nm *Runtime) LocalTunIPv4() *net.IPNet { return nm.addresses.localTunIPv4 }

// LocalTunIPv6 returns the IPv6 TUN address.
func (nm *Runtime) LocalTunIPv6() *net.IPNet { return nm.addresses.localTunIPv6 }

// rentIP allocates a TUN IP from the xds's TunConfigService via the
// already-established port-forward. Passes local interface IPs as ExcludeIPs
// so the server avoids conflicts. Retries on the rare race where a new
// interface appears between collecting addresses and receiving the allocation.
func (nm *Runtime) rentIP(ctx context.Context) error {
	conn, err := grpc.DialContext(ctx, managerTarget, nm.transport.xdsOptions()...)
	if err != nil {
		return fmt.Errorf("dial xds for RentIP: %w", err)
	}
	defer conn.Close()

	client := rpc.NewTunConfigServiceClient(conn)

	const maxRetries = 15
	for i := 0; i < maxRetries; i++ {
		generation, err := nm.transport.waitTransport(ctx)
		if err != nil {
			return err
		}
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
			if terminalTransportError(err) {
				nm.transport.signalTransportFailure(generation.id, err)
				return err
			}
			if status.Code(err) == codes.DeadlineExceeded || status.Code(err) == codes.Unavailable {
				nm.transport.signalTransportFailure(generation.id, err)
				select {
				case <-generation.ctx.Done():
				case <-ctx.Done():
					return ctx.Err()
				}
			} else {
				return err
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
			plog.G(ctx).Debugf("TUN IP %s conflicts with local interface (race), retrying (%d/%d)", v4.IP, i+1, maxRetries)
			continue
		}

		var v6 *net.IPNet
		if resp.IPv6 != "" {
			ip6, cidr6, _ := net.ParseCIDR(resp.IPv6)
			if cidr6 != nil {
				v6 = &net.IPNet{IP: ip6, Mask: cidr6.Mask}
			}
		}

		nm.addresses.localTunIPv4 = v4
		nm.addresses.localTunIPv6 = v6
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

// ChangeTunIP hot-updates the TUN device IP without restarting the network stack.
// The next heartbeat automatically uses the new IP (heartbeat reads from OS each tick).
func (nm *Runtime) ChangeTunIP(ctx context.Context, newIPv4, newIPv6 *net.IPNet) error {
	if nm.device.tunName == "" {
		return fmt.Errorf("TUN device not started: %w", config.ErrTunDeviceFailed)
	}
	if newIPv4 == nil {
		return fmt.Errorf("new IPv4 is nil: %w", config.ErrTunDeviceFailed)
	}

	oldV4, oldV6 := nm.addresses.localTunIPv4, nm.addresses.localTunIPv6
	// Always operate on host masks (/32, /128) — matching how startTUN created the
	// device. Leases now carry a host mask too (the TunConfigService stamps /32, /128),
	// so this is defensive: even if nm.addresses.localTunIPv4 ever carried a wider mask, forcing
	// /32 here keeps the delete/add aligned with the address actually on the device.
	// Only touch a family that actually changed.
	host32 := func(ip net.IP) string { return (&net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)}).String() }
	host128 := func(ip net.IP) string { return (&net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}).String() }

	if oldV4 == nil || !oldV4.IP.Equal(newIPv4.IP) {
		oldAddr := ""
		if oldV4 != nil {
			oldAddr = host32(oldV4.IP)
		}
		if err := tun.ChangeIP(nm.device.tunName, oldAddr, host32(newIPv4.IP)); err != nil {
			return fmt.Errorf("change IPv4 on %s: %w: %w", nm.device.tunName, err, config.ErrTunDeviceFailed)
		}
	}

	if newIPv6 != nil && (oldV6 == nil || !oldV6.IP.Equal(newIPv6.IP)) {
		oldAddr6 := ""
		if oldV6 != nil {
			oldAddr6 = host128(oldV6.IP)
		}
		if err := tun.ChangeIP(nm.device.tunName, oldAddr6, host128(newIPv6.IP)); err != nil {
			plog.G(ctx).Warnf("[Runtime] Change IPv6 failed: %v", err)
		}
	}

	nm.addresses.localTunIPv4 = newIPv4
	if newIPv6 != nil {
		nm.addresses.localTunIPv6 = newIPv6
	}

	// Reconcile the device's own host-route.
	if oldV4 != nil {
		_ = tun.DeleteRoutes(nm.device.tunName, types.Route{Dst: net.IPNet{IP: oldV4.IP, Mask: net.CIDRMask(32, 32)}})
	}
	if err := tun.AddRoutes(nm.device.tunName, types.Route{Dst: net.IPNet{IP: newIPv4.IP, Mask: net.CIDRMask(32, 32)}}); err != nil {
		plog.G(ctx).Warnf("[Runtime] add host route for %s failed: %v", newIPv4.IP, err)
	}
	if newIPv6 != nil {
		if oldV6 != nil {
			_ = tun.DeleteRoutes(nm.device.tunName, types.Route{Dst: net.IPNet{IP: oldV6.IP, Mask: net.CIDRMask(128, 128)}})
		}
		// Reported like the IPv4 host route above: the same failure must not be audible in one
		// family and silent in the other.
		if err := tun.AddRoutes(nm.device.tunName, types.Route{Dst: net.IPNet{IP: newIPv6.IP, Mask: net.CIDRMask(128, 128)}}); err != nil {
			plog.G(ctx).Warnf("[Runtime] add host route for %s failed: %v", newIPv6.IP, err)
		}
	}

	// On macOS the kernel stamps RT_IFA (source address) from the interface's primary
	// address at route-add time. After an IP change the old RT_IFA no longer exists on
	// the device, so outbound packets matching those routes leave with a stale source and
	// every reply is discarded. Re-asserting them makes the kernel re-derive RT_IFA.
	// Linux and Windows routes are device-scoped and unaffected.
	if runtime.GOOS == "darwin" {
		nm.refreshTunRoutes(ctx)
	}

	plog.G(ctx).Debugf("[Runtime] TUN IP changed: v4=%s v6=%s on %s", newIPv4, newIPv6, nm.device.tunName)
	return nil
}

// StartIPWatcher launches a background goroutine that connects to the xds's
// TunConfigService and watches for IP changes. When a change is detected, it calls ChangeTunIP.
// Uses the OwnerID from Config for identification.
func (nm *Runtime) StartIPWatcher(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if nm.cfg.OwnerID == "" {
		plog.G(ctx).Debug("[IPWatcher] OwnerID is empty; skipping IP watcher")
		return
	}
	nm.transport.startWatchWorker(ctx, nm.watchTunIPFromXDS)
}

func (nm *Runtime) watchTunIPFromXDS(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	plog.G(ctx).Infof("[IPWatcher] Starting IP watcher for namespace %q at %s", nm.cfg.ManagerNamespace, managerTarget)
	warnings := plog.NewThrottle(time.Minute)

	var currentVersion int64

	for ctx.Err() == nil {
		err := nm.doWatchTunIP(ctx, &currentVersion)
		if err != nil && ctx.Err() == nil {
			if warnings.Allow("retry") {
				plog.G(ctx).Warnf("[IPWatcher] %s (namespace %q) disconnected: %v, retrying in %s", managerTarget, nm.cfg.ManagerNamespace, err, ipWatcherRetryInterval)
			} else {
				plog.G(ctx).Debugf("[IPWatcher] %s (namespace %q) disconnected: %v, retrying in %s", managerTarget, nm.cfg.ManagerNamespace, err, ipWatcherRetryInterval)
			}
		}
		select {
		case <-time.After(ipWatcherRetryInterval):
		case <-ctx.Done():
			return
		}
	}
}

func (nm *Runtime) doWatchTunIP(ctx context.Context, currentVersion *int64) error {
	ctx, cancel, g, err := nm.transport.watchGeneration(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	target := g.endpoints.xds
	plog.G(ctx).Debugf("[IPWatcher] Attempting to dial xDS at %s (namespace %q, timeout %s)", target, nm.cfg.ManagerNamespace, watcherDialTimeout)
	started := time.Now()
	dialCtx, dialCancel := context.WithTimeout(ctx, watcherDialTimeout)
	opts := append(nm.transport.watchXDSOptions(g), grpc.WithBlock())
	conn, err := grpc.DialContext(dialCtx, managerTarget, opts...)
	dialCancel()
	if err != nil {
		return fmt.Errorf("dial %s after %s: %w", target, time.Since(started), err)
	}
	defer conn.Close()
	plog.G(ctx).Debugf("[IPWatcher] connected to xDS at %s in %s", target, time.Since(started))

	client := rpc.NewTunConfigServiceClient(conn)

	// Reconnect self-heal: re-assert our allocation before watching. After a long
	// disconnect the server may have reaped our lease (and won't push anything), so
	// we proactively GetTunIP. ExcludeIPs omits our OWN current TUN IP so the server
	// can hand it back (sticky via lastIPs) while still avoiding sibling/local IPs.
	resp, gerr := watchGetTunIP(ctx, client, &rpc.TunIPRequest{
		OwnerID:    nm.cfg.OwnerID,
		Namespace:  nm.cfg.ManagerNamespace,
		ExcludeIPs: nm.excludesForReassert(),
		Hostname:   nm.cfg.Hostname,
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if gerr == nil {
		if err := nm.transport.applyGeneration(ctx, g, func() error {
			v4, v6 := parseTunIPResponse(resp)
			if err := nm.applyPushedTunIP(ctx, client, v4, v6); err != nil {
				return err
			}
			*currentVersion = resp.Version
			return nil
		}); err != nil {
			return err
		}
	} else {
		plog.G(ctx).Warnf("[IPWatcher] self-heal GetTunIP at %s (namespace %q) failed: %v; continuing to WatchTunIP", target, nm.cfg.ManagerNamespace, gerr)
	}

	stream, err := client.WatchTunIP(ctx, &rpc.TunIPRequest{
		OwnerID:   nm.cfg.OwnerID,
		Namespace: nm.cfg.ManagerNamespace,
	})
	if err != nil {
		return fmt.Errorf("watch tun IP: %w", err)
	}

	for {
		resp, err := stream.Recv()
		if err != nil {
			return err
		}
		if err := nm.transport.applyGeneration(ctx, g, func() error {
			if resp.Version != *currentVersion && *currentVersion != 0 {
				v4, v6 := parseTunIPResponse(resp)
				var err error
				if resp.DryRun {
					err = nm.handleProposal(ctx, client, v4, v6)
				} else {
					err = nm.applyPushedTunIP(ctx, client, v4, v6)
				}
				if err != nil {
					return err
				}
			}
			*currentVersion = resp.Version
			return nil
		}); err != nil {
			return err
		}
	}
}

// handleProposal responds to a dry-run proposal: if the candidate is usable it
// confirms (GetTunIP{ConfirmIP}, whose committed response is then applied); if it
// conflicts locally it declines (GetTunIP{ExcludeIPs:[candidate]}), so the server
// drops the proposal without ever committing — no rollback needed.
func watchGetTunIP(ctx context.Context, client rpc.TunConfigServiceClient, req *rpc.TunIPRequest) (*rpc.TunIPResponse, error) {
	callCtx, cancel := context.WithTimeout(ctx, rentIPCallTimeout)
	defer cancel()
	return client.GetTunIP(callCtx, req)
}

func (nm *Runtime) handleProposal(ctx context.Context, client rpc.TunConfigServiceClient, v4, v6 *net.IPNet) error {
	if v4 != nil {
		if err := nm.handleProposalFamily(ctx, client, v4); err != nil {
			return err
		}
	}
	if v6 != nil {
		return nm.handleProposalFamily(ctx, client, v6)
	}
	return nil
}

// handleProposalFamily validates one proposed family candidate and confirms it
// (server then commits and returns the committed alloc to apply) or declines it.
func (nm *Runtime) handleProposalFamily(ctx context.Context, client rpc.TunConfigServiceClient, cand *net.IPNet) error {
	excludes := buildExcludeIPs(nm.cfg.ReservedTunIPs)
	var pv4, pv6 *net.IPNet
	if cand.IP.To4() == nil {
		pv6 = cand
	} else {
		pv4 = cand
	}
	if tunIPConflicts(pv4, pv6, nm.addresses.localTunIPv4, nm.addresses.localTunIPv6, excludes) {
		plog.G(ctx).Warnf("[IPWatcher] declining proposed TUN IP %s (conflicts with a local/sibling device)", cand.IP)
		rej := append(append([]string{}, excludes...), cand.IP.String())
		if _, err := watchGetTunIP(ctx, client, &rpc.TunIPRequest{
			OwnerID:    nm.cfg.OwnerID,
			Namespace:  nm.cfg.ManagerNamespace,
			ExcludeIPs: rej,
			Hostname:   nm.cfg.Hostname,
		}); err != nil {
			return err
		}
		return nil
	}
	// Usable → confirm; the committed response carries the full alloc to apply.
	resp, err := watchGetTunIP(ctx, client, &rpc.TunIPRequest{
		OwnerID:    nm.cfg.OwnerID,
		Namespace:  nm.cfg.ManagerNamespace,
		ConfirmIP:  cand.IP.String(),
		ExcludeIPs: excludes,
		Hostname:   nm.cfg.Hostname,
	})
	if err != nil {
		return err
	}
	cv4, cv6 := parseTunIPResponse(resp)
	return nm.applyPushedTunIP(ctx, client, cv4, cv6)
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
func (nm *Runtime) excludesForReassert() []string {
	own := make(map[string]bool, 2)
	if nm.addresses.localTunIPv4 != nil {
		own[nm.addresses.localTunIPv4.IP.String()] = true
	}
	if nm.addresses.localTunIPv6 != nil {
		own[nm.addresses.localTunIPv6.IP.String()] = true
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
func (nm *Runtime) applyPushedTunIP(ctx context.Context, client rpc.TunConfigServiceClient, newV4, newV6 *net.IPNet) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if newV4 == nil {
		return nil
	}
	excludes := buildExcludeIPs(nm.cfg.ReservedTunIPs)
	if tunIPConflicts(newV4, newV6, nm.addresses.localTunIPv4, nm.addresses.localTunIPv6, excludes) {
		plog.G(ctx).Warnf("[IPWatcher] pushed TUN IP %s conflicts with a local/sibling device; rejecting", newV4.IP)
		rej := append(append([]string{}, excludes...), newV4.IP.String())
		if newV6 != nil {
			rej = append(rej, newV6.IP.String())
		}
		if _, err := watchGetTunIP(ctx, client, &rpc.TunIPRequest{
			OwnerID:    nm.cfg.OwnerID,
			Namespace:  nm.cfg.ManagerNamespace,
			ExcludeIPs: rej,
			Hostname:   nm.cfg.Hostname,
		}); err != nil {
			return err
		}
		return nil
	}
	// No-op only when BOTH families already match — otherwise a v6-only change (v4
	// unchanged) would be wrongly skipped.
	v4Same := nm.addresses.localTunIPv4 != nil && nm.addresses.localTunIPv4.IP.Equal(newV4.IP)
	v6Same := newV6 == nil || (nm.addresses.localTunIPv6 != nil && nm.addresses.localTunIPv6.IP.Equal(newV6.IP))
	if v4Same && v6Same {
		return nil
	}
	return nm.ChangeTunIP(ctx, newV4, newV6)
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

// ensureTunIPAllocated allocates the TUN IP via rentIP if not already allocated.
// It emits StepStart/StepDone progress messages and logs the allocated IPs.
func (nm *Runtime) ensureTunIPAllocated(ctx context.Context) error {
	if nm.addresses.localTunIPv4 == nil {
		plog.StepStart(ctx, "Allocating TUN IP")
		if err := nm.rentIP(ctx); err != nil {
			return err
		}
		ipSummary := nm.addresses.localTunIPv4.IP.String()
		if nm.addresses.localTunIPv6 != nil {
			ipSummary += " / " + nm.addresses.localTunIPv6.IP.String()
		}
		plog.StepDone(ctx, "Allocated TUN IP %s", ipSummary)
	}
	v6Str := "<none>"
	if nm.addresses.localTunIPv6 != nil {
		v6Str = nm.addresses.localTunIPv6.IP.String()
	}
	plog.G(ctx).Debugf("IPv4: %s, IPv6: %s", nm.addresses.localTunIPv4.IP.String(), v6Str)
	return nil
}
