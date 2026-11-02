package network

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/driver"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tunnel"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// Runtime owns the networking lifecycle and coordinates address, route and DNS effects.
// Transport recovery and generation ownership belong to transportSupervisor.
type Runtime struct {
	cfg        Config
	transport  *transportSupervisor
	addresses  tunAddresses
	routeTable routeTable
	device     tunDevice
	dns        dnsState
	stopOnce   sync.Once
}

// New creates a Runtime with the given configuration.
func New(cfg Config) *Runtime {
	if cfg.RouteConfig == nil {
		cfg.RouteConfig = &RouteConfig{}
	}
	nm := &Runtime{cfg: cfg}
	nm.transport = &transportSupervisor{
		cfg:            &nm.cfg,
		changed:        make(chan struct{}),
		failures:       make(chan transportEvent, 1),
		checks:         make(chan uint64, 1),
		monitorChanges: make(chan struct{}, 1),
		heartbeatStats: &tunnel.HeartbeatStats{},
		addRoute:       nm.AddRoute,
		repairRoutes:   nm.repairStaleRouteSrc,
		routeSnapshot:  nm.routeTable.routes,
	}
	return nm
}

// LastHeartbeat returns the time of the last observed ICMP echo reply from the server gateway,
// or the zero time if none has been seen. It is the data-plane liveness signal.
func (nm *Runtime) LastHeartbeat() time.Time {
	return nm.transport.heartbeatStats.LastReply()
}

// Start brings up the full networking stack in order:
// 1. Add extra node IPs to route info
// 2. Port-forward to traffic manager (gvisor TCP/UDP + xds)
// 3. Allocate TUN IP + create local TUN device with gvisor stack
// 4. Add dynamic routes (watch pods/services)
// 5. Configure DNS
func (nm *Runtime) Start(ctx context.Context) (err error) {
	if err = nm.transport.begin(ctx); err != nil {
		return err
	}
	defer func() {
		err = nm.transport.finishStartup(err)
		if err != nil {
			nm.Stop()
		}
	}()
	startupCtx, startupCancel := context.WithTimeout(nm.transport.ctx, portForwardStartTimeout)
	stopStartup := context.AfterFunc(startupCtx, nm.transport.cancel)
	defer func() { stopStartup(); startupCancel() }()

	if err := nm.AddExtraNodeIP(nm.transport.ctx); err != nil {
		return err
	}

	plog.StepStart(nm.transport.ctx, "Forwarding ports")
	nm.transport.launchTransportSupervisor()
	generation, err := nm.transport.waitTransport(startupCtx)
	if err != nil {
		return err
	}
	plog.G(nm.transport.ctx).Debugf("Allocated local endpoints: TCP=%s, UDP=%s, xDS=%s", generation.endpoints.tcp, generation.endpoints.udp, generation.endpoints.xds)
	plog.StepDone(nm.transport.ctx, "Forwarded ports (TCP/UDP/xDS)")
	if err := nm.ensureTunIPAllocated(startupCtx); err != nil {
		return err
	}

	if util.IsWindows() {
		driver.InstallWireGuardTunDriver()
	}

	forward := "tcp://kubevpn-manager"
	if err := nm.startTUN(nm.transport.ctx, forward); err != nil {
		return fmt.Errorf("start tun: %w: %w", err, config.ErrTunDeviceFailed)
	}

	nm.transport.startNetworkMonitor(nm.device.tunName)
	nm.transport.armTransportHealth()
	if err := nm.transport.waitTransportHealthy(startupCtx); err != nil {
		return err
	}

	// Cluster pod/service CIDRs are already routed by startTUN. Per-IP pod routes and
	// service records now come from the traffic manager over WatchNamespaceRoutes
	// (StartRouteWatcher below), instead of a client-side cluster-wide list-watch.
	plog.StepDone(nm.transport.ctx, "Added %d pod/service CIDR routes", len(nm.cfg.CIDRs))

	// Pre-route the traffic manager's ClusterIP through TUN so that dialManager
	// (server-side inject) sees a consistent path from SYN onward. Without this,
	// the route watcher adds the /32 asynchronously and can split a TCP connection
	// mid-handshake: SYN via pod network, ACK via TUN → gvisor RSTs the orphan ACK.
	if err := nm.addTrafficManagerServiceRoute(nm.transport.ctx); err != nil {
		plog.G(nm.transport.ctx).Debugf("Failed to pre-route traffic manager ClusterIP: %v", err)
	}

	if err := nm.setupDNS(nm.transport.ctx); err != nil {
		return fmt.Errorf("setup dns: %w: %w", err, config.ErrDNSSetupFailed)
	}

	// Subscribe to server-side route/service discovery for the workload namespace.
	// Runs in the background under nm.transport.ctx; failures degrade to CIDR-only routing.
	nm.StartRouteWatcher(nm.transport.ctx)

	if !stopStartup() {
		return startupCtx.Err()
	}
	return nm.transport.ctx.Err()
}

// Stop tears down networking: cancels DNS, stops port-forward/informers, clears state.
func (nm *Runtime) Stop() {
	nm.stopOnce.Do(func() {
		nm.transport.stop()
		if nm.dns.dnsConfig != nil {
			nm.dns.dnsConfig.CancelDNS()
		}
		nm.device.tunName = ""
		nm.dns.dnsConfig = nil
		nm.dns.extraHost = nil
	})
}

// Done signals runtime cancellation; Stop joins the owned resources.
func (nm *Runtime) Done() <-chan struct{} {
	if nm.transport.ctx == nil {
		return nil
	}
	return nm.transport.ctx.Done()
}

// Err reports the terminal transport cause, or runtime cancellation.
func (nm *Runtime) Err() error {
	if err := nm.transport.terminalCause(); err != nil {
		return err
	}
	if nm.transport.ctx == nil {
		return nil
	}
	return nm.transport.ctx.Err()
}

// APIServerIPs returns the API endpoints excluded from cluster routing.
func (nm *Runtime) APIServerIPs() []net.IP { return nm.cfg.APIServerIPs }

// ExtraCIDRs includes configured prefixes and node addresses discovered at startup.
func (nm *Runtime) ExtraCIDRs() []string { return nm.cfg.RouteConfig.ExtraCIDR }
