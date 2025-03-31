package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"net"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/libp2p/go-netroute"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/metadata"
	v1 "k8s.io/api/core/v1"
	apinetworkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	pkgruntime "k8s.io/apimachinery/pkg/runtime"
	pkgtypes "k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/cli-runtime/pkg/resource"
	runtimeresource "k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	v2 "k8s.io/client-go/kubernetes/typed/networking/v1"
	"k8s.io/client-go/rest"
	"k8s.io/kubectl/pkg/cmd/set"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util/podutils"
	"k8s.io/utils/pointer"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dhcp"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
	"github.com/wencaiwulue/kubevpn/v2/pkg/driver"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type ConnectOptions struct {
	Namespace            string
	ExtraRouteInfo       ExtraRouteInfo
	Engine               config.Engine
	Foreground           bool
	OriginKubeconfigPath string
	Lock                 *sync.Mutex
	ImagePullSecretName  string

	ctx    context.Context
	cancel context.CancelFunc

	clientset  *kubernetes.Clientset
	restclient *rest.RESTClient
	config     *rest.Config
	factory    cmdutil.Factory
	cidrs      []*net.IPNet
	dhcp       *dhcp.Manager
	// needs to give it back to dhcp
	localTunIPv4     *net.IPNet
	localTunIPv6     *net.IPNet
	rollbackFuncList []func() error
	dnsConfig        *dns.Config

	apiServerIPs   []net.IP
	extraHost      []dns.Entry
	once           sync.Once
	tunName        string
	proxyWorkloads ProxyList
}

func (c *ConnectOptions) Context() context.Context {
	return c.ctx
}

func (c *ConnectOptions) InitDHCP(ctx context.Context) error {
	if c.dhcp == nil {
		c.dhcp = dhcp.NewDHCPManager(c.clientset.CoreV1().ConfigMaps(c.Namespace), c.Namespace)
		return c.dhcp.InitDHCP(ctx)
	}
	return nil
}

func (c *ConnectOptions) RentIP(ctx context.Context) (context.Context, error) {
	if err := c.InitDHCP(ctx); err != nil {
		return nil, err
	}
	var err error
	c.localTunIPv4, c.localTunIPv6, err = c.dhcp.RentIP(ctx)
	if err != nil {
		return nil, err
	}
	ctx1 := metadata.AppendToOutgoingContext(
		ctx,
		config.HeaderIPv4, c.localTunIPv4.String(),
		config.HeaderIPv6, c.localTunIPv6.String(),
	)
	return ctx1, nil
}

func (c *ConnectOptions) GetIPFromContext(ctx context.Context, logger *log.Logger) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return fmt.Errorf("can not get IOP from context")
	}

	ipv4 := md.Get(config.HeaderIPv4)
	if len(ipv4) == 0 {
		return fmt.Errorf("can not found IPv4 from header: %v", md)
	}
	ip, ipNet, err := net.ParseCIDR(ipv4[0])
	if err != nil {
		return fmt.Errorf("cat not convert IPv4 string: %s: %v", ipv4[0], err)
	}
	c.localTunIPv4 = &net.IPNet{IP: ip, Mask: ipNet.Mask}
	plog.G(ctx).Debugf("Get IPv4 %s from context", c.localTunIPv4.String())

	ipv6 := md.Get(config.HeaderIPv6)
	if len(ipv6) == 0 {
		return fmt.Errorf("can not found IPv6 from header: %v", md)
	}
	ip, ipNet, err = net.ParseCIDR(ipv6[0])
	if err != nil {
		return fmt.Errorf("cat not convert IPv6 string: %s: %v", ipv6[0], err)
	}
	c.localTunIPv6 = &net.IPNet{IP: ip, Mask: ipNet.Mask}
	plog.G(ctx).Debugf("Get IPv6 %s from context", c.localTunIPv6.String())
	return nil
}

func (c *ConnectOptions) CreateRemoteInboundPod(ctx context.Context, namespace string, workloads []string, headers map[string]string, portMap []string) (err error) {
	if c.localTunIPv4 == nil || c.localTunIPv6 == nil {
		return fmt.Errorf("local tun IP is invalid")
	}
	if c.proxyWorkloads == nil {
		c.proxyWorkloads = make(ProxyList, 0)
	}

	for _, workload := range workloads {
		plog.G(ctx).Infof("Injecting inbound sidecar for %s in namespace %s", workload, namespace)
		configInfo := util.PodRouteConfig{
			LocalTunIPv4: c.localTunIPv4.IP.String(),
			LocalTunIPv6: c.localTunIPv6.IP.String(),
		}
		var object *runtimeresource.Info
		object, err = util.GetUnstructuredObject(c.factory, namespace, workload)
		if err != nil {
			return err
		}
		var templateSpec *v1.PodTemplateSpec
		templateSpec, _, err = util.GetPodTemplateSpecPath(object.Object.(*unstructured.Unstructured))
		if err != nil {
			return
		}
		// todo consider to use ephemeral container
		// https://kubernetes.io/docs/concepts/workloads/pods/ephemeral-containers/
		// means mesh mode
		if c.Engine == config.EngineGvisor {
			err = inject.InjectEnvoySidecar(ctx, c.factory, c.clientset, c.Namespace, object, headers, portMap)
		} else if len(headers) != 0 || len(portMap) != 0 {
			err = inject.InjectVPNAndEnvoySidecar(ctx, c.factory, c.clientset.CoreV1().ConfigMaps(c.Namespace), c.Namespace, object, configInfo, headers, portMap)
		} else {
			err = inject.InjectVPNSidecar(ctx, c.factory, c.Namespace, object, configInfo)
		}
		if err != nil {
			plog.G(ctx).Errorf("Injecting inbound sidecar for %s in namespace %s failed: %s", workload, namespace, err.Error())
			return err
		}
		c.proxyWorkloads.Add(c.Namespace, &Proxy{
			headers:    headers,
			portMap:    portMap,
			workload:   workload,
			namespace:  namespace,
			portMapper: util.If(c.Engine == config.EngineGvisor, NewMapper(c.clientset, namespace, labels.SelectorFromSet(templateSpec.Labels).String(), headers, workload), nil),
		})
	}
	return
}

func (c *ConnectOptions) DoConnect(ctx context.Context, isLite bool, stopChan <-chan struct{}) (err error) {
	c.ctx, c.cancel = context.WithCancel(ctx)
	var success atomic.Bool
	go func() {
		// if stop chan done before current function finished, means client ctrl+c to cancel operation
		<-stopChan
		if !success.Load() {
			c.cancel()
		}
	}()

	plog.G(ctx).Info("Starting connect")
	m := dhcp.NewDHCPManager(c.clientset.CoreV1().ConfigMaps(c.Namespace), c.Namespace)
	if err = m.InitDHCP(c.ctx); err != nil {
		plog.G(ctx).Errorf("Init DHCP failed: %v", err)
		return
	}
	go c.setupSignalHandler()
	if err = c.getCIDR(c.ctx, m); err != nil {
		plog.G(ctx).Errorf("Failed to get network CIDR: %v", err)
		return
	}
	if err = createOutboundPod(c.ctx, c.factory, c.clientset, c.Namespace, c.Engine == config.EngineGvisor, c.ImagePullSecretName); err != nil {
		return
	}
	if err = c.upgradeDeploy(c.ctx); err != nil {
		return
	}
	//if err = c.CreateRemoteInboundPod(c.ctx); err != nil {
	//	return
	//}
	if err = c.addExtraNodeIP(c.ctx); err != nil {
		plog.G(ctx).Errorf("Add extra node IP failed: %v", err)
		return
	}
	var rawTCPForwardPort, gvisorTCPForwardPort, gvisorUDPForwardPort int
	rawTCPForwardPort, err = util.GetAvailableTCPPortOrDie()
	if err != nil {
		return err
	}
	gvisorTCPForwardPort, err = util.GetAvailableTCPPortOrDie()
	if err != nil {
		return err
	}
	gvisorUDPForwardPort, err = util.GetAvailableTCPPortOrDie()
	if err != nil {
		return err
	}
	plog.G(ctx).Info("Forwarding port...")
	portPair := []string{
		fmt.Sprintf("%d:10800", rawTCPForwardPort),
		fmt.Sprintf("%d:10801", gvisorTCPForwardPort),
		fmt.Sprintf("%d:10802", gvisorUDPForwardPort),
	}
	if err = c.portForward(c.ctx, portPair); err != nil {
		return
	}
	if util.IsWindows() {
		driver.InstallWireGuardTunDriver()
	}
	forward := fmt.Sprintf("tcp://127.0.0.1:%d", rawTCPForwardPort)
	if c.Engine == config.EngineGvisor {
		forward = fmt.Sprintf("tcp://127.0.0.1:%d", gvisorTCPForwardPort)
	}
	if err = c.startLocalTunServer(c.ctx, forward, isLite); err != nil {
		plog.G(ctx).Errorf("Start local tun service failed: %v", err)
		return
	}
	plog.G(ctx).Infof("Adding route...")
	if err = c.addRouteDynamic(c.ctx); err != nil {
		plog.G(ctx).Errorf("Add route dynamic failed: %v", err)
		return
	}
	go c.deleteFirewallRule(c.ctx)
	plog.G(ctx).Infof("Configuring DNS service...")
	if err = c.setupDNS(c.ctx); err != nil {
		plog.G(ctx).Errorf("Configure DNS failed: %v", err)
		return
	}
	success.Store(true)
	plog.G(ctx).Info("Configured DNS service")
	return
}

// detect pod is delete event, if pod is deleted, needs to redo port-forward immediately
func (c *ConnectOptions) portForward(ctx context.Context, portPair []string) error {
	firstCtx, firstCancelFunc := context.WithCancel(ctx)
	defer firstCancelFunc()
	var errChan = make(chan error, 1)
	go func() {
		runtime.ErrorHandlers = runtime.ErrorHandlers[0:0]
		var first = pointer.Bool(true)
		for ctx.Err() == nil {
			func() {
				defer time.Sleep(time.Millisecond * 200)

				sortBy := func(pods []*v1.Pod) sort.Interface { return sort.Reverse(podutils.ActivePods(pods)) }
				label := fields.OneTermEqualSelector("app", config.ConfigMapPodTrafficManager).String()
				_, _, _ = polymorphichelpers.GetFirstPod(c.clientset.CoreV1(), c.Namespace, label, time.Second*5, sortBy)
				ctx2, cancelFunc2 := context.WithTimeout(ctx, time.Second*5)
				defer cancelFunc2()
				podList, err := c.GetRunningPodList(ctx2)
				if err != nil {
					plog.G(ctx).Debugf("Failed to get running pod: %v", err)
					if *first {
						util.SafeWrite(errChan, err)
					}
					return
				}
				pod := podList[0]
				// add route in case of don't have permission to watch pod, but pod recreated ip changed, so maybe this ip can not visit
				_ = c.addRoute(pod.Status.PodIP)
				childCtx, cancelFunc := context.WithCancel(ctx)
				defer cancelFunc()
				var readyChan = make(chan struct{})
				podName := pod.GetName()
				// try to detect pod is delete event, if pod is deleted, needs to redo port-forward
				go util.CheckPodStatus(childCtx, cancelFunc, podName, c.clientset.CoreV1().Pods(c.Namespace))
				go util.CheckPortStatus(childCtx, cancelFunc, readyChan, strings.Split(portPair[1], ":")[0])
				if *first {
					go func() {
						select {
						case <-readyChan:
							firstCancelFunc()
						case <-childCtx.Done():
						}
					}()
				}
				var out = plog.G(ctx).Out
				err = util.PortForwardPod(
					c.config,
					c.restclient,
					podName,
					c.Namespace,
					portPair,
					readyChan,
					childCtx.Done(),
					out,
					out,
				)
				if *first {
					util.SafeWrite(errChan, err)
				}
				first = pointer.Bool(false)
				// exit normal, let context.err to judge to exit or not
				if err == nil {
					plog.G(ctx).Debugf("Port forward retrying")
					return
				} else {
					plog.G(ctx).Debugf("Forward port error: %v", err)
				}
				if strings.Contains(err.Error(), "unable to listen on any of the requested ports") ||
					strings.Contains(err.Error(), "address already in use") {
					plog.G(ctx).Debugf("Port %s already in use, needs to release it manually", portPair)
				} else {
					plog.G(ctx).Debugf("Port-forward occurs error: %v", err)
				}
			}()
		}
	}()
	ticker := time.NewTicker(time.Second * 60)
	defer ticker.Stop()
	select {
	case <-ticker.C:
		return errors.New("wait port forward to be ready timeout")
	case err := <-errChan:
		return err
	case <-firstCtx.Done():
		return nil
	}
}

func (c *ConnectOptions) startLocalTunServer(ctx context.Context, forwardAddress string, lite bool) (err error) {
	plog.G(ctx).Debugf("IPv4: %s, IPv6: %s", c.localTunIPv4.IP.String(), c.localTunIPv6.IP.String())

	var cidrList []*net.IPNet
	if !lite {
		cidrList = append(cidrList, config.CIDR, config.CIDR6)
	} else {
		// windows needs to add tun IP self to route table, but linux and macOS not need
		if util.IsWindows() {
			cidrList = append(cidrList,
				&net.IPNet{IP: c.localTunIPv4.IP, Mask: net.CIDRMask(32, 32)},
				&net.IPNet{IP: c.localTunIPv6.IP, Mask: net.CIDRMask(128, 128)},
			)
		}
	}
	for _, ipNet := range c.cidrs {
		cidrList = append(cidrList, ipNet)
	}
	// add extra-cidr
	for _, s := range c.ExtraRouteInfo.ExtraCIDR {
		var ipnet *net.IPNet
		_, ipnet, err = net.ParseCIDR(s)
		if err != nil {
			return fmt.Errorf("invalid extra-cidr %s, err: %v", s, err)
		}
		cidrList = append(cidrList, ipnet)
	}

	var routes []types.Route
	for _, ipNet := range util.RemoveLargerOverlappingCIDRs(cidrList) {
		routes = append(routes, types.Route{Dst: *ipNet})
	}

	tunConfig := tun.Config{
		Addr:   (&net.IPNet{IP: c.localTunIPv4.IP, Mask: net.CIDRMask(32, 32)}).String(),
		Routes: routes,
		MTU:    config.DefaultMTU,
	}
	if enable, _ := util.IsIPv6Enabled(); enable {
		tunConfig.Addr6 = (&net.IPNet{IP: c.localTunIPv6.IP, Mask: net.CIDRMask(128, 128)}).String()
	}

	localNode := fmt.Sprintf("tun:/127.0.0.1:8422")
	node, err := core.ParseNode(localNode)
	if err != nil {
		plog.G(ctx).Errorf("Failed to parse local node %s: %v", localNode, err)
		return err
	}

	chainNode, err := core.ParseNode(forwardAddress)
	if err != nil {
		plog.G(ctx).Errorf("Failed to parse forward node %s: %v", forwardAddress, err)
		return err
	}
	chainNode.Client = &core.Client{
		Connector:   core.UDPOverTCPTunnelConnector(),
		Transporter: core.TCPTransporter(),
	}
	chain := core.NewChain(5, chainNode)

	handler := core.TunHandler(chain, node)
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
		defer server.Listener.Close()
		go func() {
			<-ctx.Done()
			server.Listener.Close()
		}()

		for ctx.Err() == nil {
			conn, err := server.Listener.Accept()
			if err != nil {
				if !errors.Is(err, tun.ClosedErr) {
					plog.G(ctx).Errorf("Failed to accept local tun conn: %v", err)
				}
				return
			}
			go server.Handler.Handle(ctx, conn)
		}
	}()
	plog.G(ctx).Info("Connected tunnel")

	c.tunName, err = c.GetTunDeviceName()
	return err
}

// Listen all pod, add route if needed
func (c *ConnectOptions) addRouteDynamic(ctx context.Context) error {
	podNs, svcNs, err1 := util.GetNsForListPodAndSvc(ctx, c.clientset, []string{v1.NamespaceAll, c.Namespace})
	if err1 != nil {
		return err1
	}

	go func() {
		var listDone bool
		for ctx.Err() == nil {
			err := func() error {
				if !listDone {
					err := util.ListService(ctx, c.clientset.CoreV1().Services(svcNs), c.addRoute)
					if err != nil {
						return err
					}
					listDone = true
				}
				err := util.WatchServiceToAddRoute(ctx, c.clientset.CoreV1().Services(svcNs), c.addRoute)
				return err
			}()
			if utilnet.IsConnectionRefused(err) || apierrors.IsTooManyRequests(err) || apierrors.IsForbidden(err) {
				time.Sleep(time.Second * 10)
			} else {
				time.Sleep(time.Second * 2)
			}
		}
	}()

	go func() {
		var listDone bool
		for ctx.Err() == nil {
			err := func() error {
				if !listDone {
					err := util.ListPod(ctx, c.clientset.CoreV1().Pods(podNs), c.addRoute)
					if err != nil {
						return err
					}
					listDone = true
				}
				err := util.WatchPodToAddRoute(ctx, c.clientset.CoreV1().Pods(podNs), c.addRoute)
				return err
			}()
			if utilnet.IsConnectionRefused(err) || apierrors.IsTooManyRequests(err) || apierrors.IsForbidden(err) {
				time.Sleep(time.Second * 10)
			} else {
				time.Sleep(time.Second * 2)
			}
		}
	}()

	return nil
}

func (c *ConnectOptions) addRoute(ipStrList ...string) error {
	if c.tunName == "" {
		return nil
	}
	var routes []types.Route
	for _, ipStr := range ipStrList {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		var match bool
		for _, p := range c.apiServerIPs {
			// if pod ip or service ip is equal to apiServer ip, can not add it to route table
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
		if r, err := netroute.New(); err == nil {
			ifi, _, _, err := r.Route(ip)
			if err == nil && ifi.Name == c.tunName {
				continue
			}
		}
		routes = append(routes, types.Route{Dst: net.IPNet{IP: ip, Mask: mask}})
	}
	err := tun.AddRoutes(c.tunName, routes...)
	return err
}

func (c *ConnectOptions) deleteFirewallRule(ctx context.Context) {
	if !util.IsWindows() {
		return
	}
	// The reason why delete firewall rule is:
	// On windows ping local tun IPv4/v6 not works
	// so needs to add firewall rule to allow this
	if !util.FindAllowFirewallRule(ctx) {
		util.AddAllowFirewallRule(ctx)
	}

	// The reason why delete firewall rule is:
	// On windows use 'kubevpn proxy deploy/authors -H user=windows'
	// Open terminal 'curl localhost:9080' ok
	// Open terminal 'curl localTunIP:9080' not ok
	util.DeleteBlockFirewallRule(ctx)
}

func (c *ConnectOptions) setupDNS(ctx context.Context) error {
	const portTCP = 10800
	podList, err := c.GetRunningPodList(ctx)
	if err != nil {
		plog.G(ctx).Errorf("Get running pod list failed, err: %v", err)
		return err
	}
	pod := podList[0]
	plog.G(ctx).Debugf("Get DNS service IP from pod...")
	relovConf, err := util.GetDNSServiceIPFromPod(ctx, c.clientset, c.config, pod.GetName(), c.Namespace)
	if err != nil {
		plog.G(ctx).Errorln(err)
		return err
	}

	marshal, _ := json.Marshal(relovConf)
	plog.G(ctx).Debugf("Get DNS service config: %v", string(marshal))
	svc, err := c.clientset.CoreV1().Services(c.Namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return err
	}

	var conn net.Conn
	d := net.Dialer{Timeout: time.Duration(max(2, relovConf.Timeout)) * time.Second}
	conn, err = d.DialContext(ctx, "tcp", net.JoinHostPort(svc.Spec.ClusterIP, strconv.Itoa(portTCP)))
	if err != nil {
		relovConf.Servers = []string{pod.Status.PodIP}
		err = nil
		plog.G(ctx).Debugf("DNS service use pod IP %s", pod.Status.PodIP)
	} else {
		relovConf.Servers = []string{svc.Spec.ClusterIP}
		_ = conn.Close()
		plog.G(ctx).Debugf("DNS service use service IP %s", svc.Spec.ClusterIP)
	}

	plog.G(ctx).Debugf("Adding extra hosts...")
	if err = c.addExtraRoute(c.ctx, pod.GetName()); err != nil {
		plog.G(ctx).Errorf("Add extra route failed: %v", err)
		return err
	}

	ns := []string{c.Namespace}
	list, err := c.clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, item := range list.Items {
			if !sets.New[string](ns...).Has(item.Name) {
				ns = append(ns, item.Name)
			}
		}
	}

	var serviceList []v1.Service
	services, err := c.clientset.CoreV1().Services(c.Namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		serviceList = append(serviceList, services.Items...)
	}

	c.dnsConfig = &dns.Config{
		Config:   relovConf,
		Ns:       ns,
		Services: serviceList,
		TunName:  c.tunName,
		Hosts:    c.extraHost,
		Lock:     c.Lock,
		HowToGetExternalName: func(domain string) (string, error) {
			podList, err := c.GetRunningPodList(ctx)
			if err != nil {
				return "", err
			}
			pod := podList[0]
			return util.Shell(
				ctx,
				c.clientset,
				c.config,
				pod.GetName(),
				config.ContainerSidecarVPN,
				c.Namespace,
				[]string{"dig", "+short", domain},
			)
		},
	}
	plog.G(ctx).Debugf("Setup DNS...")
	if err = c.dnsConfig.SetupDNS(ctx); err != nil {
		return err
	}
	plog.G(ctx).Debugf("Dump service in namespace %s into hosts...", c.Namespace)
	// dump service in current namespace for support DNS resolve service:port
	err = c.dnsConfig.AddServiceNameToHosts(ctx, c.clientset.CoreV1().Services(c.Namespace), c.extraHost...)
	return err
}

func Run(ctx context.Context, servers []core.Server) error {
	errChan := make(chan error, len(servers))
	for i := range servers {
		go func(i int) {
			errChan <- func() error {
				svr := servers[i]
				defer svr.Listener.Close()
				go func() {
					<-ctx.Done()
					svr.Listener.Close()
				}()
				for ctx.Err() == nil {
					conn, err := svr.Listener.Accept()
					if err != nil {
						return err
					}
					go svr.Handler.Handle(ctx, conn)
				}
				return ctx.Err()
			}()
		}(i)
	}

	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func Parse(r core.Route) ([]core.Server, error) {
	servers, err := r.GenerateServers()
	if err != nil {
		return nil, err
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("server is empty, server config: %s", strings.Join(r.ServeNodes, ","))
	}
	return servers, nil
}

func (c *ConnectOptions) InitClient(f cmdutil.Factory) (err error) {
	c.factory = f
	if c.config, err = c.factory.ToRESTConfig(); err != nil {
		return
	}
	if c.restclient, err = c.factory.RESTClient(); err != nil {
		return
	}
	if c.clientset, err = c.factory.KubernetesClientSet(); err != nil {
		return
	}
	if c.Namespace, _, err = c.factory.ToRawKubeConfigLoader().Namespace(); err != nil {
		return
	}
	return
}

func (c *ConnectOptions) GetRunningPodList(ctx context.Context) ([]v1.Pod, error) {
	label := fields.OneTermEqualSelector("app", config.ConfigMapPodTrafficManager).String()
	return util.GetRunningPodList(ctx, c.clientset, c.Namespace, label)
}

// getCIDR
// 1: get pod cidr
// 2: get service cidr
// distinguish service cidr and pod cidr
// https://stackoverflow.com/questions/45903123/kubernetes-set-service-cidr-and-pod-cidr-the-same
// https://stackoverflow.com/questions/44190607/how-do-you-find-the-cluster-service-cidr-of-a-kubernetes-cluster/54183373#54183373
// https://stackoverflow.com/questions/44190607/how-do-you-find-the-cluster-service-cidr-of-a-kubernetes-cluster
func (c *ConnectOptions) getCIDR(ctx context.Context, m *dhcp.Manager) (err error) {
	defer func() {
		if err == nil {
			u, err2 := url.Parse(c.config.Host)
			if err2 != nil {
				return
			}
			host, _, err3 := net.SplitHostPort(u.Host)
			if err3 != nil {
				return
			}
			var ipList []net.IP
			if ip := net.ParseIP(host); ip != nil {
				ipList = append(ipList, ip)
			}
			ips, _ := net.LookupIP(host)
			if ips != nil {
				ipList = append(ipList, ips...)
			}
			c.apiServerIPs = ipList
			c.removeCIDRsContainingIPs(ipList)
		}
	}()

	// (1) get CIDR from cache
	var value string
	value, err = m.Get(ctx, config.KeyClusterIPv4POOLS)
	if err == nil {
		for _, s := range strings.Split(value, " ") {
			_, cidr, _ := net.ParseCIDR(s)
			if cidr != nil {
				c.cidrs = util.RemoveLargerOverlappingCIDRs(append(c.cidrs, cidr))
			}
		}
		if len(c.cidrs) != 0 {
			plog.G(ctx).Infoln("Got network CIDR from cache")
			return nil
		}
	}

	// (2) get CIDR from cni
	c.cidrs, err = util.GetCIDRElegant(ctx, c.clientset, c.config, c.Namespace)
	if err == nil {
		s := sets.New[string]()
		for _, cidr := range c.cidrs {
			s.Insert(cidr.String())
		}
		cidrs := util.GetCIDRFromResourceUgly(ctx, c.clientset, c.Namespace)
		for _, cidr := range cidrs {
			s.Insert(cidr.String())
		}
		c.cidrs = util.RemoveLargerOverlappingCIDRs(append(c.cidrs, cidrs...))
		_ = m.Set(ctx, config.KeyClusterIPv4POOLS, strings.Join(s.UnsortedList(), " "))
		return nil
	}

	// (3) fallback to get cidr from node/pod/service
	c.cidrs = util.GetCIDRFromResourceUgly(ctx, c.clientset, c.Namespace)
	return nil
}

func (c *ConnectOptions) removeCIDRsContainingIPs(ipList []net.IP) {
	for i := len(c.cidrs) - 1; i >= 0; i-- {
		for _, ip := range ipList {
			if c.cidrs[i].Contains(ip) {
				c.cidrs = append(c.cidrs[:i], c.cidrs[i+1:]...)
				break
			}
		}
	}
}

func (c *ConnectOptions) addExtraRoute(ctx context.Context, name string) error {
	if len(c.ExtraRouteInfo.ExtraDomain) == 0 {
		return nil
	}

	// 1) use dig +short query, if ok, just return
	for _, domain := range c.ExtraRouteInfo.ExtraDomain {
		ip, err := util.Shell(ctx, c.clientset, c.config, name, config.ContainerSidecarVPN, c.Namespace, []string{"dig", "+short", domain})
		if err != nil {
			return errors.WithMessage(err, "failed to resolve DNS for domain by command dig")
		}
		// try to get ingress record
		if net.ParseIP(ip) == nil {
			ip = getIngressRecord(ctx, c.clientset.NetworkingV1(), []string{v1.NamespaceAll, c.Namespace}, domain)
		}
		if net.ParseIP(ip) == nil {
			return fmt.Errorf("failed to resolve DNS for domain %s by command dig", domain)
		}
		err = c.addRoute(ip)
		if err != nil {
			plog.G(ctx).Errorf("Failed to add IP: %s to route table: %v", ip, err)
			return err
		}
		c.extraHost = append(c.extraHost, dns.Entry{IP: net.ParseIP(ip).String(), Domain: domain})
	}
	return nil
}

func getIngressRecord(ctx context.Context, ingressInterface v2.NetworkingV1Interface, nsList []string, domain string) string {
	var ingressList []apinetworkingv1.Ingress
	for _, ns := range nsList {
		list, _ := ingressInterface.Ingresses(ns).List(ctx, metav1.ListOptions{})
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

func (c *ConnectOptions) addExtraNodeIP(ctx context.Context) error {
	if !c.ExtraRouteInfo.ExtraNodeIP {
		return nil
	}
	list, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
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
				c.ExtraRouteInfo.ExtraCIDR = append(c.ExtraRouteInfo.ExtraCIDR, (&net.IPNet{
					IP:   ip,
					Mask: mask,
				}).String())
			}
		}
	}
	return nil
}

func (c *ConnectOptions) GetClientset() *kubernetes.Clientset {
	return c.clientset
}

func (c *ConnectOptions) GetFactory() cmdutil.Factory {
	return c.factory
}

func (c *ConnectOptions) GetLocalTunIP() (v4 string, v6 string) {
	if c.localTunIPv4 != nil {
		v4 = c.localTunIPv4.IP.String()
	}
	if c.localTunIPv6 != nil {
		v6 = c.localTunIPv6.IP.String()
	}
	return
}

func (c *ConnectOptions) GetClusterID() string {
	if c != nil && c.dhcp != nil {
		return string(c.dhcp.GetClusterID())
	}
	return ""
}

func (c *ConnectOptions) upgradeDeploy(ctx context.Context) error {
	deploy, err := c.clientset.AppsV1().Deployments(c.Namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if len(deploy.Spec.Template.Spec.Containers) == 0 {
		return fmt.Errorf("can not found any container in deploy %s", deploy.Name)
	}

	clientVer := config.Version
	clientImg := config.Image
	serverImg := deploy.Spec.Template.Spec.Containers[0].Image

	isNeedUpgrade, err := util.IsNewer(clientVer, clientImg, serverImg)
	if !isNeedUpgrade {
		return nil
	}
	if err != nil {
		return err
	}

	plog.G(ctx).Infof("Set image %s --> %s...", serverImg, clientImg)

	err = upgradeDeploySpec(ctx, c.factory, c.Namespace, deploy.Name, c.Engine == config.EngineGvisor)
	if err != nil {
		return err
	}
	// because use webhook(kubevpn-traffic-manager container webhook) to assign ip,
	// if create new pod use old webhook, ip will still change to old CIDR.
	// so after patched, check again if env is newer or not,
	// if env is still old, needs to re-patch using new webhook
	err = restartDeploy(ctx, c.factory, c.clientset, c.Namespace, deploy.Name)
	if err != nil {
		return err
	}
	return nil
}

func upgradeDeploySpec(ctx context.Context, f cmdutil.Factory, ns, name string, gvisor bool) error {
	r := f.NewBuilder().
		WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
		NamespaceParam(ns).DefaultNamespace().
		ResourceNames("deployments", name).
		ContinueOnError().
		Latest().
		Flatten().
		Do()
	if err := r.Err(); err != nil {
		return err
	}
	infos, err := r.Infos()
	if err != nil {
		return err
	}
	// issue: https://github.com/kubernetes/kubernetes/issues/98963
	for _, info := range infos {
		_, _ = polymorphichelpers.UpdatePodSpecForObjectFn(info.Object, func(spec *v1.PodSpec) error {
			for i := 0; i < len(spec.ImagePullSecrets); i++ {
				if spec.ImagePullSecrets[i].Name == "" {
					spec.ImagePullSecrets = append(spec.ImagePullSecrets[:i], spec.ImagePullSecrets[i+1:]...)
					i--
					continue
				}
			}
			if len(spec.ImagePullSecrets) == 0 {
				spec.ImagePullSecrets = nil
			}
			return nil
		})
	}
	patches := set.CalculatePatches(infos, scheme.DefaultJSONEncoder(), func(obj pkgruntime.Object) ([]byte, error) {
		_, err = polymorphichelpers.UpdatePodSpecForObjectFn(obj, func(spec *v1.PodSpec) error {
			udp8422 := "8422-for-udp"
			tcp10800 := "10800-for-tcp"
			tcp9002 := "9002-for-envoy"
			tcp80 := "80-for-webhook"
			udp53 := "53-for-dns"
			var imagePullSecret string
			for _, secret := range spec.ImagePullSecrets {
				if secret.Name != "" {
					imagePullSecret = secret.Name
					break
				}
			}
			deploySpec := genDeploySpec(ns, udp8422, tcp10800, tcp9002, udp53, tcp80, gvisor, imagePullSecret)
			*spec = deploySpec.Spec.Template.Spec
			return nil
		})
		if err != nil {
			return nil, err
		}
		return pkgruntime.Encode(scheme.DefaultJSONEncoder(), obj)
	})
	for _, p := range patches {
		if p.Err != nil {
			return p.Err
		}
	}
	for _, p := range patches {
		_, err = resource.
			NewHelper(p.Info.Client, p.Info.Mapping).
			DryRun(false).
			Patch(p.Info.Namespace, p.Info.Name, pkgtypes.StrategicMergePatchType, p.Patch, nil)
		if err != nil {
			plog.G(ctx).Errorf("Failed to patch image update to pod template: %v", err)
			return err
		}
		err = util.RolloutStatus(ctx, f, ns, fmt.Sprintf("%s/%s", p.Info.Mapping.Resource.GroupResource().String(), p.Info.Name), time.Minute*60)
		if err != nil {
			return err
		}
	}
	return nil
}

func restartDeploy(ctx context.Context, f cmdutil.Factory, clientset *kubernetes.Clientset, ns, name string) error {
	label := fields.OneTermEqualSelector("app", config.ConfigMapPodTrafficManager).String()
	list, err := util.GetRunningPodList(ctx, clientset, ns, label)
	if err != nil {
		return err
	}
	pod := list[0]
	container, _ := util.FindContainerByName(&pod, config.ContainerSidecarVPN)
	if container == nil {
		return nil
	}

	envs := map[string]string{
		"CIDR4":                     config.CIDR.String(),
		"CIDR6":                     config.CIDR6.String(),
		config.EnvInboundPodTunIPv4: (&net.IPNet{IP: config.RouterIP, Mask: config.CIDR.Mask}).String(),
		config.EnvInboundPodTunIPv6: (&net.IPNet{IP: config.RouterIP6, Mask: config.CIDR6.Mask}).String(),
	}

	var mismatch bool
	for _, existing := range container.Env {
		if envs[existing.Name] != existing.Value {
			mismatch = true
			break
		}
	}
	if !mismatch {
		return nil
	}
	err = deletePodImmediately(ctx, clientset, ns, label)
	if err != nil {
		return err
	}
	err = util.RolloutStatus(ctx, f, ns, fmt.Sprintf("%s/%s", "deployments", name), time.Minute*60)
	return err
}

// delete old pod immediately
func deletePodImmediately(ctx context.Context, clientset *kubernetes.Clientset, ns string, label string) error {
	result, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: label,
	})
	if err != nil {
		return err
	}
	// delete old pod then delete new pod
	sort.SliceStable(result.Items, func(i, j int) bool {
		return result.Items[i].DeletionTimestamp != nil
	})
	for _, item := range result.Items {
		options := metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)}
		err = clientset.CoreV1().Pods(ns).Delete(ctx, item.Name, options)
		if apierrors.IsNotFound(err) {
			err = nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *ConnectOptions) Equal(a *ConnectOptions) bool {
	return c.Engine == a.Engine &&
		sets.New[string](c.ExtraRouteInfo.ExtraDomain...).HasAll(a.ExtraRouteInfo.ExtraDomain...) &&
		sets.New[string](c.ExtraRouteInfo.ExtraCIDR...).HasAll(c.ExtraRouteInfo.ExtraCIDR...) &&
		(reflect.DeepEqual(c.ExtraRouteInfo.ExtraNodeIP, a.ExtraRouteInfo.ExtraNodeIP) || c.ExtraRouteInfo.ExtraNodeIP == true)
}

func (c *ConnectOptions) GetTunDeviceName() (string, error) {
	var ips []net.IP
	if c.localTunIPv4 != nil {
		ips = append(ips, c.localTunIPv4.IP)
	}
	if c.localTunIPv6 != nil {
		ips = append(ips, c.localTunIPv6.IP)
	}
	device, err := util.GetTunDevice(ips...)
	if err != nil {
		return "", err
	}
	return device.Name, nil
}

func (c *ConnectOptions) AddRolloutFunc(f func() error) {
	c.rollbackFuncList = append(c.rollbackFuncList, f)
}

func (c *ConnectOptions) getRolloutFunc() []func() error {
	return c.rollbackFuncList
}

func (c *ConnectOptions) LeavePortMap(ns, workload string) {
	c.proxyWorkloads.Remove(ns, workload)
}

func (c *ConnectOptions) IsMe(ns, uid string, headers map[string]string) bool {
	return c.proxyWorkloads.IsMe(ns, uid, headers)
}

func (c *ConnectOptions) ProxyResources() ProxyList {
	return c.proxyWorkloads
}
