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
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	informerv1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	v2 "k8s.io/client-go/kubernetes/typed/networking/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

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

	// GetRunningPodList returns running traffic manager pods.
	GetRunningPodList func(ctx context.Context) ([]v1.Pod, error)
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
}

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

	plog.G(nm.ctx).Info("Forwarding port...")
	portPair := []string{
		fmt.Sprintf("%d:10801", gvisorTCPForwardPort),
		fmt.Sprintf("%d:10802", gvisorUDPForwardPort),
		fmt.Sprintf("%d:%d", controlPlanePort, config.PortControlPlane),
	}

	if err := nm.portForward(nm.ctx, portPair); err != nil {
		return err
	}

	if util.IsWindows() {
		driver.InstallWireGuardTunDriver()
	}

	forward := fmt.Sprintf("tcp://127.0.0.1:%d", gvisorTCPForwardPort)
	if err := nm.startTUN(nm.ctx, forward); err != nil {
		return err
	}

	plog.G(nm.ctx).Infof("Adding Pod IP and Service IP to route table...")
	svcInformer, _, err := nm.AddRouteDynamic(nm.ctx)
	if err != nil {
		return err
	}

	plog.G(nm.ctx).Infof("Configuring DNS service...")
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
			ExcludeIPs: collectLocalIPs(),
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
		plog.G(ctx).Infof("Allocated TUN IP: v4=%s v6=%s", v4, v6)
		return nil
	}

	return fmt.Errorf("failed to allocate a non-conflicting TUN IP after %d attempts", maxRetries)
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

	oldAddr := ""
	if nm.localTunIPv4 != nil {
		oldAddr = nm.localTunIPv4.String()
	}
	if err := tun.ChangeIP(nm.tunName, oldAddr, newIPv4.String()); err != nil {
		return fmt.Errorf("change IPv4 on %s: %w", nm.tunName, err)
	}

	if newIPv6 != nil && nm.localTunIPv6 != nil {
		oldAddr6 := nm.localTunIPv6.String()
		if err := tun.ChangeIP(nm.tunName, oldAddr6, newIPv6.String()); err != nil {
			plog.G(ctx).Warnf("[NetworkManager] Change IPv6 failed: %v", err)
		}
	}

	nm.localTunIPv4 = newIPv4
	if newIPv6 != nil {
		nm.localTunIPv6 = newIPv6
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
		case <-time.After(10 * time.Second):
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
			if newV4 != nil {
				if changeErr := nm.ChangeTunIP(ctx, newV4, newV6); changeErr != nil {
					plog.G(ctx).Errorf("[IPWatcher] ChangeTunIP failed: %v", changeErr)
				}
			}
		}
		*currentVersion = resp.Version
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
		runtime.ErrorHandlers = []runtime.ErrorHandler{func(ctx context.Context, err error, msg string, keysAndValues ...any) {
			plog.G(ctx).Error(err)
		}}
		first := ptr.To(true)
		for ctx.Err() == nil {
			sessionStart := time.Now()
			err := nm.portForwardOnce(ctx, portPair, *first, firstCancelFunc)
			sessionDuration := time.Since(sessionStart)
			if *first {
				if err != nil {
					util.SafeWrite(errChan, err)
					return
				}
			} else {
				plog.G(ctx).Infof("[Perf] Port-forward session ended after %v, reconnecting...", sessionDuration)
			}
			first = ptr.To(false)
			time.Sleep(time.Millisecond * 200)
		}
	}()
	ticker := time.NewTicker(time.Second * 60)
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
	ctx2, cancelFunc2 := context.WithTimeout(ctx, time.Second*10)
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
	go healthCheckTCPConn(childCtx, cancelFunc, readyChan, controlPlanePort, nm.cfg.OwnerID, nm.cfg.ManagerNamespace)
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
	for _, ipNet := range nm.cfg.CIDRs {
		cidrList = append(cidrList, ipNet)
	}
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
		if err := nm.rentIP(ctx); err != nil {
			return err
		}
	}
	plog.G(ctx).Debugf("IPv4: %s, IPv6: %s", nm.localTunIPv4.IP.String(), nm.localTunIPv6.IP.String())

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
	if enable, _ := util.IsIPv6Enabled(); enable {
		tunConfig.Addr6 = (&net.IPNet{IP: nm.localTunIPv6.IP, Mask: net.CIDRMask(128, 128)}).String()
	}

	forwarder := &core.Forwarder{
		Addr:        forwardNode.Addr,
		Connector:   core.NewUDPOverTCPConnector(),
		Transporter: core.TCPTransporter(tlsSecret.Data),
		MaxRetries:  5,
	}

	handler := core.TunHandler(forwarder, core.NewRouteHub())
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
	plog.G(ctx).Infof("[Client] TUN server started, forwarding to %s", forwardAddress)

	nm.tunName, err = nm.getTunDeviceName()
	return err
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
	plog.G(ctx).Infof("Get DNS service IP from Pod...")
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

	plog.G(ctx).Infof("Adding extra domain to hosts...")
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

	plog.G(ctx).Infof("Listing namespace %s services...", nm.cfg.WorkloadNamespace)
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
	plog.G(ctx).Infof("Setup DNS server for device %s...", nm.tunName)
	if err = nm.dnsConfig.SetupDNS(ctx); err != nil {
		return err
	}
	plog.G(ctx).Infof("Dump service in namespace %s into hosts...", nm.cfg.WorkloadNamespace)
	// dump service in current namespace for support DNS resolve service:port
	err = nm.dnsConfig.AddServiceNameToHosts(ctx, nm.extraHost...)
	return err
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
	ticker := time.NewTicker(time.Second * 15)
	_, err := informer.AddEventHandler(newTickerResetHandler(ticker))
	if err != nil {
		return err
	}
	go informer.Run(ctx.Done())
	go func() {
		defer ticker.Stop()
		for ; ctx.Err() == nil; <-ticker.C {
			ticker.Reset(time.Second * 15)
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
