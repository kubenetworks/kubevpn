package network

import (
	"context"
	"fmt"
	"net"

	"github.com/containernetworking/cni/pkg/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tunnel"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

type tunDevice struct {
	tunName        string
	newTunListener func(tun.Config) (net.Listener, error)
}

// TunName returns the TUN device name (empty if not started).
func (nm *Runtime) TunName() string {
	return nm.device.tunName
}

// startTUN allocates a TUN IP and creates the local TUN device with a gvisor network stack.
func (nm *Runtime) startTUN(ctx context.Context, forwardAddress string) error {
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
	for _, s := range nm.cfg.RouteConfig.ExtraCIDR {
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
	routes = append(routes, tunHostRoutes(nm.addresses.localTunIPv4, nm.addresses.localTunIPv6)...)

	tunConfig := buildTunConfig(nm.addresses.localTunIPv4, nm.addresses.localTunIPv6, routes)

	return nm.startTunServer(ctx, forwardNode, forwardAddress, tlsSecret.Data, tunConfig)
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
func (nm *Runtime) startTunServer(ctx context.Context, forwardNode *core.Node, forwardAddress string, secretData map[string][]byte, tunConfig tun.Config) error {
	forwarder := &core.Forwarder{
		Addr:        forwardNode.Addr,
		Connector:   core.NewUDPOverTCPConnector(),
		Transporter: &generationTransporter{nm: nm.transport, base: core.TCPTransporter(secretData), endpoint: endpointTCP},
		MaxRetries:  5,
	}

	plog.StepStart(ctx, "Creating TUN device")
	// heartbeatStats is allocated once in New; reuse it (do not re-assign) so the
	// liveness watchdog and status queries observe the same instance without a data race.
	handler := tunnel.TunHandler(forwarder.TunnelConfig(), core.NewRouteHub(), nm.transport.heartbeatStats)
	newListener := nm.device.newTunListener
	if newListener == nil {
		newListener = tun.Listener
	}
	listener, err := newListener(tunConfig)
	if err != nil {
		plog.G(ctx).Errorf("Failed to create tun listener: %v", err)
		return fmt.Errorf("create tun listener: %w: %w", err, config.ErrTunDeviceFailed)
	}

	server := core.Server{
		Listener: listener,
		Handler:  &tunCompletionHandler{Handler: handler, cancel: nm.transport.cancel},
	}

	if !nm.transport.startWorker(func() {
		if err := core.Serve(ctx, []core.Server{server}); err != nil && ctx.Err() == nil {
			plog.G(ctx).Errorf("[Client] Local TUN server exited: %v", err)
			nm.transport.cancel()
		}
	}) {
		listener.Close()
		return net.ErrClosed
	}
	plog.G(ctx).Debugf("[Client] TUN server started, forwarding to %s", forwardAddress)

	nm.device.tunName, err = nm.getTunDeviceName()
	if err != nil {
		listener.Close()
		return err
	}
	plog.StepDone(ctx, "Created TUN device %q", nm.device.tunName)
	return nil
}

// getTunDeviceName resolves the TUN device name from the configured IPs.
func (nm *Runtime) getTunDeviceName() (string, error) {
	var ips []net.IP
	if nm.addresses.localTunIPv4 != nil {
		ips = append(ips, nm.addresses.localTunIPv4.IP)
	}
	if nm.addresses.localTunIPv6 != nil {
		ips = append(ips, nm.addresses.localTunIPv6.IP)
	}
	device, err := netutil.GetTunDevice(ips...)
	if err != nil {
		return "", err
	}
	return device.Name, nil
}

// A TUN handler exiting is terminal even if its listener's Accept is still
// blocked. Cancel the manager; its external owner performs joined cleanup.
type tunCompletionHandler struct {
	core.Handler
	cancel context.CancelFunc
}

func (h *tunCompletionHandler) Handle(ctx context.Context, c net.Conn) {
	defer h.cancel()
	h.Handler.Handle(ctx, c)
}
