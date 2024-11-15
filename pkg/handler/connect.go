package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/url"
	"os/exec"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/distribution/reference"
	goversion "github.com/hashicorp/go-version"
	"github.com/libp2p/go-netroute"
	miekgdns "github.com/miekg/dns"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/metadata"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	pkgruntime "k8s.io/apimachinery/pkg/runtime"
	pkgtypes "k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubectl/pkg/cmd/set"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util/podutils"
	"k8s.io/utils/pointer"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dhcp"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
	"github.com/wencaiwulue/kubevpn/v2/pkg/driver"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type ConnectOptions struct {
	Namespace            string
	Headers              map[string]string
	PortMap              []string
	Workloads            []string
	ExtraRouteInfo       ExtraRouteInfo
	Engine               config.Engine
	Foreground           bool
	OriginKubeconfigPath string
	Lock                 *sync.Mutex

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

	apiServerIPs []net.IP
	extraHost    []dns.Entry
	once         sync.Once
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

func (c *ConnectOptions) GetIPFromContext(ctx context.Context) error {
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
	log.Debugf("Get IPv4 %s from context", c.localTunIPv4.String())

	ipv6 := md.Get(config.HeaderIPv6)
	if len(ipv6) == 0 {
		return fmt.Errorf("can not found IPv6 from header: %v", md)
	}
	ip, ipNet, err = net.ParseCIDR(ipv6[0])
	if err != nil {
		return fmt.Errorf("cat not convert IPv6 string: %s: %v", ipv6[0], err)
	}
	c.localTunIPv6 = &net.IPNet{IP: ip, Mask: ipNet.Mask}
	log.Debugf("Get IPv6 %s from context", c.localTunIPv6.String())
	return nil
}

func (c *ConnectOptions) CreateRemoteInboundPod(ctx context.Context) (err error) {
	if c.localTunIPv4 == nil || c.localTunIPv6 == nil {
		return fmt.Errorf("local tun IP is invalid")
	}

	for _, workload := range c.Workloads {
		log.Infof("Injecting inbound sidecar for %s", workload)
		configInfo := util.PodRouteConfig{
			LocalTunIPv4: c.localTunIPv4.IP.String(),
			LocalTunIPv6: c.localTunIPv6.IP.String(),
		}
		// todo consider to use ephemeral container
		// https://kubernetes.io/docs/concepts/workloads/pods/ephemeral-containers/
		// means mesh mode
		if len(c.Headers) != 0 || len(c.PortMap) != 0 {
			err = inject.InjectVPNAndEnvoySidecar(ctx, c.factory, c.clientset.CoreV1().ConfigMaps(c.Namespace), c.Namespace, workload, configInfo, c.Headers, c.PortMap)
		} else {
			err = inject.InjectVPNSidecar(ctx, c.factory, c.Namespace, workload, configInfo)
		}
		if err != nil {
			log.Errorf("Injecting inbound sidecar for %s failed: %s", workload, err.Error())
			return err
		}
	}
	return
}

func (c *ConnectOptions) DoConnect(ctx context.Context, isLite bool) (err error) {
	c.ctx, c.cancel = context.WithCancel(ctx)

	log.Info("Starting connect")
	m := dhcp.NewDHCPManager(c.clientset.CoreV1().ConfigMaps(c.Namespace), c.Namespace)
	if err = m.InitDHCP(c.ctx); err != nil {
		log.Errorf("Init DHCP failed: %v", err)
		return
	}
	go c.setupSignalHandler()
	if err = c.getCIDR(c.ctx, m); err != nil {
		log.Errorf("Failed to get network CIDR: %v", err)
		return
	}
	if err = createOutboundPod(c.ctx, c.factory, c.clientset, c.Namespace); err != nil {
		return
	}
	if err = c.upgradeDeploy(c.ctx); err != nil {
		return
	}
	//if err = c.CreateRemoteInboundPod(c.ctx); err != nil {
	//	return
	//}
	if err = c.addExtraNodeIP(c.ctx); err != nil {
		log.Errorf("Add extra node IP failed: %v", err)
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
	log.Info("Forwarding port...")
	if err = c.portForward(c.ctx, []string{
		fmt.Sprintf("%d:10800", rawTCPForwardPort),
		fmt.Sprintf("%d:10801", gvisorTCPForwardPort),
		fmt.Sprintf("%d:10802", gvisorUDPForwardPort),
	}); err != nil {
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
		log.Errorf("Start local tun service failed: %v", err)
		return
	}
	log.Infof("Adding route...")
	if err = c.addRouteDynamic(c.ctx); err != nil {
		log.Errorf("Add route dynamic failed: %v", err)
		return
	}
	go c.deleteFirewallRule(c.ctx)
	log.Infof("Configuring DNS service...")
	if err = c.setupDNS(c.ctx); err != nil {
		log.Errorf("Configure DNS failed: %v", err)
		return
	}
	log.Info("Configured DNS service")
	return
}

// detect pod is delete event, if pod is deleted, needs to redo port-forward immediately
func (c *ConnectOptions) portForward(ctx context.Context, portPair []string) error {
	firstCtx, firstCancelFunc := context.WithCancel(ctx)
	defer firstCancelFunc()
	var errChan = make(chan error, 1)
	go func() {
		runtime.ErrorHandlers = []func(error){}
		var first = pointer.Bool(true)
		for ctx.Err() == nil {
			func() {
				defer time.Sleep(time.Millisecond * 200)

				sortBy := func(pods []*v1.Pod) sort.Interface { return sort.Reverse(podutils.ActivePods(pods)) }
				label := fields.OneTermEqualSelector("app", config.ConfigMapPodTrafficManager).String()
				_, _, _ = polymorphichelpers.GetFirstPod(c.clientset.CoreV1(), c.Namespace, label, time.Second*10, sortBy)
				ctx2, cancelFunc2 := context.WithTimeout(ctx, time.Second*10)
				defer cancelFunc2()
				podList, err := c.GetRunningPodList(ctx2)
				if err != nil {
					log.Debugf("Failed to get running pod: %v", err)
					if *first {
						util.SafeWrite(errChan, err)
					}
					return
				}
				childCtx, cancelFunc := context.WithCancel(ctx)
				defer cancelFunc()
				var readyChan = make(chan struct{})
				podName := podList[0].GetName()
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
				var out = log.StandardLogger().WriterLevel(log.DebugLevel)
				defer out.Close()
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
					log.Debugf("Port forward retrying")
					return
				}
				if strings.Contains(err.Error(), "unable to listen on any of the requested ports") ||
					strings.Contains(err.Error(), "address already in use") {
					log.Debugf("Port %s already in use, needs to release it manually", portPair)
				} else {
					log.Debugf("Port-forward occurs error: %v", err)
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
	log.Debugf("IPv4: %s, IPv6: %s", c.localTunIPv4.IP.String(), c.localTunIPv6.IP.String())

	var cidrList []*net.IPNet
	if !lite {
		cidrList = append(cidrList, config.CIDR)
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
		Addr:   c.localTunIPv4.String(),
		Routes: routes,
	}
	if enable, _ := util.IsIPv6Enabled(); enable {
		tunConfig.Addr6 = c.localTunIPv6.String()
	}

	localNode := fmt.Sprintf("tun:/127.0.0.1:8422")
	node, err := core.ParseNode(localNode)
	if err != nil {
		log.Errorf("Failed to parse local node %s: %v", localNode, err)
		return err
	}
	node.Values.Add(config.ConfigKubeVPNTransportEngine, string(c.Engine))

	chainNode, err := core.ParseNode(forwardAddress)
	if err != nil {
		log.Errorf("Failed to parse forward node %s: %v", forwardAddress, err)
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
		log.Errorf("Failed to create tun listener: %v", err)
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
					log.Errorf("Failed to accept local tun conn: %v", err)
				}
				return
			}
			go server.Handler.Handle(ctx, conn)
		}
	}()
	log.Info("Connected tunnel")
	return
}

// Listen all pod, add route if needed
func (c *ConnectOptions) addRouteDynamic(ctx context.Context) error {
	tunName, e := c.GetTunDeviceName()
	if e != nil {
		return e
	}

	podNs, svcNs, err1 := util.GetNsForListPodAndSvc(ctx, c.clientset, []string{v1.NamespaceAll, c.Namespace})
	if err1 != nil {
		return err1
	}

	var addRouteFunc = func(resource, ipStr string) {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			return
		}
		for _, p := range c.apiServerIPs {
			// if pod ip or service ip is equal to apiServer ip, can not add it to route table
			if p.Equal(ip) {
				return
			}
		}

		var mask net.IPMask
		if ip.To4() != nil {
			mask = net.CIDRMask(32, 32)
		} else {
			mask = net.CIDRMask(128, 128)
		}
		if r, err := netroute.New(); err == nil {
			iface, _, _, err := r.Route(ip)
			if err == nil && iface.Name == tunName {
				return
			}
		}
		errs := tun.AddRoutes(tunName, types.Route{Dst: net.IPNet{IP: ip, Mask: mask}})
		if errs != nil {
			log.Errorf("Failed to add route, resource: %s, IP: %s, err: %v", resource, ip, errs)
		}
	}

	go func() {
		var listDone bool
		for ctx.Err() == nil {
			err := func() error {
				if !listDone {
					err := util.ListService(ctx, c.clientset.CoreV1().Services(svcNs), addRouteFunc)
					if err != nil {
						return err
					}
					listDone = true
				}
				err := util.WatchServiceToAddRoute(ctx, c.clientset.CoreV1().Services(svcNs), addRouteFunc)
				return err
			}()
			if utilnet.IsConnectionRefused(err) || apierrors.IsTooManyRequests(err) || apierrors.IsForbidden(err) {
				time.Sleep(time.Second * 1)
			} else {
				time.Sleep(time.Millisecond * 200)
			}
		}
	}()

	go func() {
		var listDone bool
		for ctx.Err() == nil {
			err := func() error {
				if !listDone {
					err := util.ListPod(ctx, c.clientset.CoreV1().Pods(podNs), addRouteFunc)
					if err != nil {
						return err
					}
					listDone = true
				}
				err := util.WatchPodToAddRoute(ctx, c.clientset.CoreV1().Pods(podNs), addRouteFunc)
				return err
			}()
			if utilnet.IsConnectionRefused(err) || apierrors.IsTooManyRequests(err) || apierrors.IsForbidden(err) {
				time.Sleep(time.Second * 1)
			} else {
				time.Sleep(time.Millisecond * 200)
			}
		}
	}()

	return nil
}

func (c *ConnectOptions) deleteFirewallRule(ctx context.Context) {
	// The reason why delete firewall rule is:
	// On windows use 'kubevpn proxy deploy/authors -H user=windows'
	// Open terminal 'curl localhost:9080' ok
	// Open terminal 'curl localTunIP:9080' not ok
	util.DeleteBlockFirewallRule(ctx)
}

func (c *ConnectOptions) setupDNS(ctx context.Context) error {
	const port = 53
	const portTCP = 10800
	pod, err := c.GetRunningPodList(ctx)
	if err != nil {
		log.Errorf("Get running pod list failed, err: %v", err)
		return err
	}
	log.Debugf("Get DNS service IP from pod...")
	relovConf, err := util.GetDNSServiceIPFromPod(ctx, c.clientset, c.config, pod[0].GetName(), c.Namespace)
	if err != nil {
		log.Errorln(err)
		return err
	}
	if relovConf.Port == "" {
		relovConf.Port = strconv.Itoa(port)
	}

	marshal, _ := json.Marshal(relovConf)
	log.Debugf("Get DNS service config: %v", string(marshal))
	svc, err := c.clientset.CoreV1().Services(c.Namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return err
	}

	var conn net.Conn
	d := net.Dialer{Timeout: time.Duration(max(2, relovConf.Timeout)) * time.Second}
	conn, err = d.DialContext(ctx, "tcp", net.JoinHostPort(svc.Spec.ClusterIP, strconv.Itoa(portTCP)))
	if err != nil {
		relovConf.Servers = []string{pod[0].Status.PodIP}
		err = nil
		log.Debugf("DNS service use pod IP %s", pod[0].Status.PodIP)
	} else {
		relovConf.Servers = []string{svc.Spec.ClusterIP}
		_ = conn.Close()
		log.Debugf("DNS service use service IP %s", svc.Spec.ClusterIP)
	}

	log.Debugf("Adding extra hosts...")
	if err = c.addExtraRoute(c.ctx, relovConf.Servers[0]); err != nil {
		log.Errorf("Add extra route failed: %v", err)
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
	tunName, err := c.GetTunDeviceName()
	if err != nil {
		return err
	}
	c.dnsConfig = &dns.Config{
		Config:  relovConf,
		Ns:      ns,
		TunName: tunName,
		Hosts:   c.extraHost,
		Lock:    c.Lock,
	}
	log.Debugf("Setup DNS...")
	if err = c.dnsConfig.SetupDNS(ctx); err != nil {
		return err
	}
	log.Debugf("Dump service in namespace %s into hosts...", c.Namespace)
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

// PreCheckResource transform user parameter to normal, example:
// pod: productpage-7667dfcddb-cbsn5
// replicast: productpage-7667dfcddb
// deployment: productpage
// transform:
// pod/productpage-7667dfcddb-cbsn5 --> deployment/productpage
// service/productpage --> deployment/productpage
// replicaset/productpage-7667dfcddb --> deployment/productpage
//
// pods without controller
// pod/productpage-without-controller --> pod/productpage-without-controller
// service/productpage-without-pod --> controller/controllerName
func (c *ConnectOptions) PreCheckResource() error {
	if len(c.Workloads) == 0 {
		return nil
	}

	list, err := util.GetUnstructuredObjectList(c.factory, c.Namespace, c.Workloads)
	if err != nil {
		return err
	}
	var resources []string
	for _, info := range list {
		resources = append(resources, fmt.Sprintf("%s/%s", info.Mapping.GroupVersionKind.GroupKind().String(), info.Name))
	}
	c.Workloads = resources

	// normal workloads, like pod with controller, deployments, statefulset, replicaset etc...
	for i, workload := range c.Workloads {
		ownerReference, err := util.GetTopOwnerReference(c.factory, c.Namespace, workload)
		if err == nil {
			c.Workloads[i] = fmt.Sprintf("%s/%s", ownerReference.Mapping.GroupVersionKind.GroupKind().String(), ownerReference.Name)
		}
	}
	// service which associate with pod
	for i, workload := range c.Workloads {
		object, err := util.GetUnstructuredObject(c.factory, c.Namespace, workload)
		if err != nil {
			return err
		}
		if object.Mapping.Resource.Resource != "services" {
			continue
		}
		get, err := c.clientset.CoreV1().Services(c.Namespace).Get(context.Background(), object.Name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		if ns, selector, err := polymorphichelpers.SelectorsForObject(get); err == nil {
			list, err := c.clientset.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{
				LabelSelector: selector.String(),
			})
			// if pod is not empty, using pods to find top controller
			if err == nil && list != nil && len(list.Items) != 0 {
				ownerReference, err := util.GetTopOwnerReference(c.factory, c.Namespace, fmt.Sprintf("%s/%s", "pods", list.Items[0].Name))
				if err == nil {
					c.Workloads[i] = fmt.Sprintf("%s/%s", ownerReference.Mapping.GroupVersionKind.GroupKind().String(), ownerReference.Name)
				}
			} else
			// if list is empty, means not create pods, just controllers
			{
				controller, err := util.GetTopOwnerReferenceBySelector(c.factory, c.Namespace, selector.String())
				if err == nil {
					if len(controller) > 0 {
						c.Workloads[i] = controller.UnsortedList()[0]
					}
				}
				// only a single service, not support it yet
				if controller.Len() == 0 {
					return fmt.Errorf("not support resources: %s", workload)
				}
			}
		}
	}
	return nil
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
			for i := 0; i < len(c.cidrs); i++ {
				for _, ip := range ipList {
					if c.cidrs[i].Contains(ip) {
						c.cidrs = append(c.cidrs[:i], c.cidrs[i+1:]...)
						i--
					}
				}
			}
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
			log.Infoln("Got network CIDR from cache")
			return
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
		return
	}

	// (3) fallback to get cidr from node/pod/service
	c.cidrs = util.GetCIDRFromResourceUgly(ctx, c.clientset, c.Namespace)
	return
}

func (c *ConnectOptions) addExtraRoute(ctx context.Context, nameserver string) error {
	if len(c.ExtraRouteInfo.ExtraDomain) == 0 {
		return nil
	}

	tunName, err := c.GetTunDeviceName()
	if err != nil {
		log.Errorf("Get tun interface failed: %s", err.Error())
		return err
	}

	addRouteFunc := func(resource, ip string) {
		if net.ParseIP(ip) == nil {
			return
		}
		var mask net.IPMask
		if net.ParseIP(ip).To4() != nil {
			mask = net.CIDRMask(32, 32)
		} else {
			mask = net.CIDRMask(128, 128)
		}
		errs := tun.AddRoutes(tunName, types.Route{Dst: net.IPNet{IP: net.ParseIP(ip), Mask: mask}})
		if errs != nil {
			log.Errorf("Failed to add route, domain: %s, IP: %s, err: %v", resource, ip, err)
		}
	}

	// 1) use dig +short query, if ok, just return
	podList, err := c.GetRunningPodList(ctx)
	if err != nil {
		return err
	}
	var ok = true
	for _, domain := range c.ExtraRouteInfo.ExtraDomain {
		ip, err := util.Shell(ctx, c.clientset, c.config, podList[0].Name, config.ContainerSidecarVPN, c.Namespace, []string{"dig", "+short", domain})
		if err == nil || net.ParseIP(ip) != nil {
			addRouteFunc(domain, ip)
			c.extraHost = append(c.extraHost, dns.Entry{IP: net.ParseIP(ip).String(), Domain: domain})
		} else {
			ok = false
		}
	}
	if ok {
		return nil
	}

	// 2) wait until can ping dns server ip ok
	// 3) use nslookup to query dns at first, it will speed up mikdns query process
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	go func() {
		for _, domain := range c.ExtraRouteInfo.ExtraDomain {
			go func(domain string) {
				for ; true; <-ticker.C {
					func() {
						subCtx, c2 := context.WithTimeout(ctx, time.Second*2)
						defer c2()
						cmd := exec.CommandContext(subCtx, "nslookup", domain, nameserver)
						cmd.Stderr = io.Discard
						cmd.Stdout = io.Discard
						_ = cmd.Start()
						_ = cmd.Wait()
					}()
				}
			}(domain)
		}
	}()

	// 4) query with dns client
	client := &miekgdns.Client{Net: "udp", Timeout: time.Second * 2}
	for _, domain := range c.ExtraRouteInfo.ExtraDomain {
		var success = false
		for _, qType := range []uint16{miekgdns.TypeA /*, miekgdns.TypeAAAA*/} {
			var iErr = errors.New("No retry")
			err = retry.OnError(
				wait.Backoff{
					Steps:    1000,
					Duration: time.Millisecond * 30,
				},
				func(err error) bool {
					return err != nil
				},
				func() error {
					var answer *miekgdns.Msg
					answer, _, err = client.ExchangeContext(ctx, &miekgdns.Msg{
						MsgHdr: miekgdns.MsgHdr{
							Id: uint16(rand.Intn(math.MaxUint16 + 1)),
						},
						Question: []miekgdns.Question{
							{
								Name:  domain + ".",
								Qtype: qType,
							},
						},
					}, fmt.Sprintf("%s:%d", nameserver, 53))
					if err != nil {
						return err
					}
					if len(answer.Answer) == 0 {
						return iErr
					}
					for _, rr := range answer.Answer {
						switch a := rr.(type) {
						case *miekgdns.A:
							if ip := net.ParseIP(a.A.String()); ip != nil && !ip.IsLoopback() {
								addRouteFunc(domain, a.A.String())
								c.extraHost = append(c.extraHost, dns.Entry{IP: a.A.String(), Domain: domain})
								success = true
							}
						case *miekgdns.AAAA:
							if ip := net.ParseIP(a.AAAA.String()); ip != nil && !ip.IsLoopback() {
								addRouteFunc(domain, a.AAAA.String())
								c.extraHost = append(c.extraHost, dns.Entry{IP: a.AAAA.String(), Domain: domain})
								success = true
							}
						}
					}
					return nil
				})
			if err != nil && err != iErr {
				return err
			}
			if success {
				break
			}
		}
		if !success {
			return fmt.Errorf("failed to resolve DNS for domain %s", domain)
		}
	}
	return nil
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

	clientImg := config.Image
	serverImg := deploy.Spec.Template.Spec.Containers[0].Image

	if clientImg == serverImg {
		return nil
	}

	isNewer, _ := newer(clientImg, serverImg)
	if deploy.Status.ReadyReplicas > 0 && !isNewer {
		return nil
	}

	log.Infof("Set image %s --> %s...", serverImg, clientImg)

	r := c.factory.NewBuilder().
		WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
		NamespaceParam(c.Namespace).DefaultNamespace().
		ResourceNames("deployments", deploy.Name).
		ContinueOnError().
		Latest().
		Flatten().
		Do()
	if err = r.Err(); err != nil {
		return err
	}
	infos, err := r.Infos()
	if err != nil {
		return err
	}
	patches := set.CalculatePatches(infos, scheme.DefaultJSONEncoder(), func(obj pkgruntime.Object) ([]byte, error) {
		_, err = polymorphichelpers.UpdatePodSpecForObjectFn(obj, func(spec *v1.PodSpec) error {
			for i := range spec.Containers {
				spec.Containers[i].Image = clientImg
			}
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
			log.Errorf("Failed to patch image update to pod template: %v", err)
			return err
		}
		err = util.RolloutStatus(ctx, c.factory, c.Namespace, fmt.Sprintf("%s/%s", p.Info.Mapping.Resource.GroupResource().String(), p.Info.Name), time.Minute*60)
		if err != nil {
			return err
		}
	}
	return nil
}

func newer(clientImgStr, serverImgStr string) (bool, error) {
	clientImg, err := reference.ParseNormalizedNamed(clientImgStr)
	if err != nil {
		return false, err
	}
	serverImg, err := reference.ParseNormalizedNamed(serverImgStr)
	if err != nil {
		return false, err
	}
	if reference.Domain(clientImg) != reference.Domain(serverImg) {
		return false, nil
	}

	serverTag, ok := serverImg.(reference.NamedTagged)
	if !ok {
		return false, fmt.Errorf("can not convert server image")
	}
	serverVersion, err := goversion.NewVersion(serverTag.Tag())
	if err != nil {
		return false, err
	}
	clientTag, ok := clientImg.(reference.NamedTagged)
	if !ok {
		return false, fmt.Errorf("can not convert client image")
	}
	clientVersion, err := goversion.NewVersion(clientTag.Tag())
	if err != nil {
		return false, err
	}
	return clientVersion.GreaterThan(serverVersion), nil
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
