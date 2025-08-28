package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/libp2p/go-netroute"
	miekgdns "github.com/miekg/dns"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/metadata"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	v1 "k8s.io/api/core/v1"
	apinetworkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	pkgruntime "k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	pkgtypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/resource"
	runtimeresource "k8s.io/cli-runtime/pkg/resource"
	informerv1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	v2 "k8s.io/client-go/kubernetes/typed/networking/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubectl/pkg/cmd/set"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util/podutils"
	"k8s.io/utils/pointer"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
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
	Foreground           bool
	OriginKubeconfigPath string
	OriginNamespace      string
	Lock                 *sync.Mutex
	Image                string
	ImagePullSecretName  string
	Request              *rpc.ConnectRequest `json:"Request,omitempty"`

	ctx    context.Context
	cancel context.CancelFunc

	clientset  *kubernetes.Clientset
	restclient *rest.RESTClient
	config     *rest.Config
	factory    cmdutil.Factory
	cidrs      []*net.IPNet
	dhcp       *dhcp.Manager
	// needs to give it back to dhcp
	LocalTunIPv4     *net.IPNet `json:"LocalTunIPv4,omitempty"`
	LocalTunIPv6     *net.IPNet `json:"LocalTunIPv6,omitempty"`
	rollbackFuncList []func() error
	dnsConfig        *dns.Config

	apiServerIPs   []net.IP
	extraHost      []dns.Entry
	once           sync.Once
	tunName        string
	proxyWorkloads ProxyList

	Sync *SyncOptions
}

func (c *ConnectOptions) Context() context.Context {
	return c.ctx
}

func (c *ConnectOptions) InitDHCP(ctx context.Context) error {
	if c.dhcp == nil {
		c.dhcp = dhcp.NewDHCPManager(c.clientset, c.Namespace)
		return c.dhcp.InitDHCP(ctx)
	}
	return nil
}

func (c *ConnectOptions) RentIP(ctx context.Context, ipv4, ipv6 string) (context.Context, error) {
	if err := c.InitDHCP(ctx); err != nil {
		return nil, err
	}
	if util.IsValidCIDR(ipv4) && util.IsValidCIDR(ipv6) {
		ip, cidr, _ := net.ParseCIDR(ipv4)
		c.LocalTunIPv4 = &net.IPNet{IP: ip, Mask: cidr.Mask}
		ip, cidr, _ = net.ParseCIDR(ipv6)
		c.LocalTunIPv6 = &net.IPNet{IP: ip, Mask: cidr.Mask}
	} else {
		var err error
		c.LocalTunIPv4, c.LocalTunIPv6, err = c.dhcp.RentIP(ctx)
		if err != nil {
			return nil, err
		}
	}

	return metadata.AppendToOutgoingContext(
		context.Background(),
		config.HeaderIPv4, c.LocalTunIPv4.String(),
		config.HeaderIPv6, c.LocalTunIPv6.String(),
	), nil
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
	c.LocalTunIPv4 = &net.IPNet{IP: ip, Mask: ipNet.Mask}
	logger.Debugf("Get IPv4 %s from context", c.LocalTunIPv4.String())

	ipv6 := md.Get(config.HeaderIPv6)
	if len(ipv6) == 0 {
		return fmt.Errorf("can not found IPv6 from header: %v", md)
	}
	ip, ipNet, err = net.ParseCIDR(ipv6[0])
	if err != nil {
		return fmt.Errorf("cat not convert IPv6 string: %s: %v", ipv6[0], err)
	}
	c.LocalTunIPv6 = &net.IPNet{IP: ip, Mask: ipNet.Mask}
	logger.Debugf("Get IPv6 %s from context", c.LocalTunIPv6.String())
	return nil
}

func (c *ConnectOptions) CreateRemoteInboundPod(ctx context.Context, namespace string, workloads []string, headers map[string]string, portMap []string, image string) (err error) {
	if c.LocalTunIPv4 == nil || c.LocalTunIPv6 == nil {
		return fmt.Errorf("local tun IP is invalid")
	}
	if c.proxyWorkloads == nil {
		c.proxyWorkloads = make(ProxyList, 0)
	}

	tlsSecret, err := c.clientset.CoreV1().Secrets(c.Namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return err
	}

	for _, workload := range workloads {
		plog.G(ctx).Infof("Injecting inbound sidecar for %s in namespace %s", workload, namespace)
		configInfo := util.PodRouteConfig{
			LocalTunIPv4: c.LocalTunIPv4.IP.String(),
			LocalTunIPv6: c.LocalTunIPv6.IP.String(),
		}
		var object, controller *runtimeresource.Info
		object, controller, err = util.GetTopOwnerObject(ctx, c.factory, namespace, workload)
		if err != nil {
			return err
		}
		var templateSpec *v1.PodTemplateSpec
		templateSpec, _, err = util.GetPodTemplateSpecPath(controller.Object.(*unstructured.Unstructured))
		if err != nil {
			return
		}
		nodeID := fmt.Sprintf("%s.%s", object.Mapping.Resource.GroupResource().String(), object.Name)
		// todo consider to use ephemeral container
		// https://kubernetes.io/docs/concepts/workloads/pods/ephemeral-containers/
		// means mesh mode
		if util.IsK8sService(object) {
			err = inject.InjectEnvoyAndSSH(ctx, nodeID, c.factory, c.Namespace, object, controller, headers, portMap, image)
		} else if len(headers) != 0 || len(portMap) != 0 {
			err = inject.InjectServiceMesh(ctx, nodeID, c.factory, c.Namespace, controller, configInfo, headers, portMap, tlsSecret, image)
		} else {
			err = inject.InjectVPN(ctx, nodeID, c.factory, c.Namespace, controller, configInfo, tlsSecret, image)
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
			portMapper: util.If(util.IsK8sService(object), NewMapper(c.clientset, namespace, labels.SelectorFromSet(templateSpec.Labels).String(), headers, workload), nil),
		})
	}
	return
}

func (c *ConnectOptions) DoConnect(ctx context.Context) (err error) {
	c.ctx, c.cancel = context.WithCancel(ctx)
	plog.G(ctx).Info("Starting connect to cluster")
	if err = c.InitDHCP(c.ctx); err != nil {
		plog.G(ctx).Errorf("Init DHCP server failed: %v", err)
		return
	}
	go c.setupSignalHandler()
	if err = c.getCIDR(c.ctx, true); err != nil {
		plog.G(ctx).Errorf("Failed to get network CIDR: %v", err)
		return
	}
	if err = createOutboundPod(c.ctx, c.clientset, c.Namespace, c.Image, c.ImagePullSecretName); err != nil {
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
	var gvisorTCPForwardPort, gvisorUDPForwardPort int
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
		fmt.Sprintf("%d:10801", gvisorTCPForwardPort),
		fmt.Sprintf("%d:10802", gvisorUDPForwardPort),
	}
	if err = c.portForward(c.ctx, portPair); err != nil {
		return
	}
	if util.IsWindows() {
		driver.InstallWireGuardTunDriver()
	}
	forward := fmt.Sprintf("tcp://127.0.0.1:%d", gvisorTCPForwardPort)

	if err = c.startLocalTunServer(c.ctx, forward); err != nil {
		plog.G(ctx).Errorf("Start local tun service failed: %v", err)
		return
	}
	plog.G(ctx).Infof("Adding Pod IP and Service IP to route table...")
	var svcInformer cache.SharedIndexInformer
	if svcInformer, _, err = c.addRouteDynamic(c.ctx); err != nil {
		plog.G(ctx).Errorf("Add route dynamic failed: %v", err)
		return
	}
	plog.G(ctx).Infof("Configuring DNS service...")
	if err = c.setupDNS(c.ctx, svcInformer); err != nil {
		plog.G(ctx).Errorf("Configure DNS failed: %v", err)
		return
	}
	return
}

// detect pod is delete event, if pod is deleted, needs to redo port-forward immediately
func (c *ConnectOptions) portForward(ctx context.Context, portPair []string) error {
	firstCtx, firstCancelFunc := context.WithCancel(ctx)
	defer firstCancelFunc()
	var errChan = make(chan error, 1)
	go func() {
		runtime.ErrorHandlers = []runtime.ErrorHandler{func(ctx context.Context, err error, msg string, keysAndValues ...interface{}) {
			plog.G(ctx).Error(err)
		}}
		var first = pointer.Bool(true)
		for ctx.Err() == nil {
			func() {
				defer time.Sleep(time.Millisecond * 200)

				sortBy := func(pods []*v1.Pod) sort.Interface { return sort.Reverse(podutils.ActivePods(pods)) }
				label := fields.OneTermEqualSelector("app", config.ConfigMapPodTrafficManager).String()
				_, _, _ = polymorphichelpers.GetFirstPod(c.clientset.CoreV1(), c.Namespace, label, time.Second*5, sortBy)
				ctx2, cancelFunc2 := context.WithTimeout(ctx, time.Second*10)
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
				_ = c.addRoute(util.GetPodIP(pod)...)
				childCtx, cancelFunc := context.WithCancel(ctx)
				defer cancelFunc()
				var readyChan = make(chan struct{})
				podName := pod.GetName()
				// try to detect pod is delete event, if pod is deleted, needs to redo port-forward
				go util.CheckPodStatus(childCtx, cancelFunc, podName, c.clientset.CoreV1().Pods(c.Namespace))
				domain := fmt.Sprintf("%s.%s", config.ConfigMapPodTrafficManager, c.Namespace)
				go healthCheckPortForward(childCtx, cancelFunc, readyChan, strings.Split(portPair[1], ":")[0], domain, c.LocalTunIPv4.IP)
				go healthCheckTCPConn(childCtx, cancelFunc, readyChan, domain, util.GetPodIP(pod)[0])
				if *first {
					go func() {
						select {
						case <-readyChan:
							firstCancelFunc()
						case <-childCtx.Done():
						}
					}()
				}
				out := plog.G(ctx).Out
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

func (c *ConnectOptions) startLocalTunServer(ctx context.Context, forwardAddress string) (err error) {
	plog.G(ctx).Debugf("IPv4: %s, IPv6: %s", c.LocalTunIPv4.IP.String(), c.LocalTunIPv6.IP.String())

	tlsSecret, err := c.clientset.CoreV1().Secrets(c.Namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return err
	}

	var cidrList []*net.IPNet
	for _, ipNet := range c.cidrs {
		cidrList = append(cidrList, ipNet)
	}
	// add extra-cidr
	for _, s := range c.ExtraRouteInfo.ExtraCIDR {
		var ipNet *net.IPNet
		_, ipNet, err = net.ParseCIDR(s)
		if err != nil {
			return fmt.Errorf("invalid extra-cidr %s, err: %v", s, err)
		}
		cidrList = append(cidrList, ipNet)
	}

	var routes []types.Route
	for _, ipNet := range util.RemoveCIDRsContainingIPs(util.RemoveLargerOverlappingCIDRs(cidrList), c.apiServerIPs) {
		if ipNet != nil {
			routes = append(routes, types.Route{Dst: *ipNet})
		}
	}
	if c.LocalTunIPv4 != nil {
		routes = append(routes, types.Route{Dst: net.IPNet{IP: c.LocalTunIPv4.IP, Mask: net.CIDRMask(32, 32)}})
	}
	if c.LocalTunIPv6 != nil {
		routes = append(routes, types.Route{Dst: net.IPNet{IP: c.LocalTunIPv6.IP, Mask: net.CIDRMask(128, 128)}})
	}

	tunConfig := tun.Config{
		Addr:   (&net.IPNet{IP: c.LocalTunIPv4.IP, Mask: net.CIDRMask(32, 32)}).String(),
		Routes: routes,
		MTU:    config.DefaultMTU,
	}
	if enable, _ := util.IsIPv6Enabled(); enable {
		tunConfig.Addr6 = (&net.IPNet{IP: c.LocalTunIPv6.IP, Mask: net.CIDRMask(128, 128)}).String()
	}

	localNode := "tun://"
	node, err := core.ParseNode(localNode)
	if err != nil {
		plog.G(ctx).Errorf("Failed to parse local node %s: %v", localNode, err)
		return err
	}
	forward, err := core.ParseNode(forwardAddress)
	if err != nil {
		plog.G(ctx).Errorf("Failed to parse forward node %s: %v", forwardAddress, err)
		return err
	}
	forward.Client = &core.Client{
		Connector:   core.NewUDPOverTCPConnector(),
		Transporter: core.TCPTransporter(tlsSecret.Data),
	}
	forwarder := core.NewForwarder(5, forward)

	handler := core.TunHandler(node, forwarder)
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
	plog.G(ctx).Info("Connected private safe tunnel")

	c.tunName, err = c.GetTunDeviceName()
	return err
}

// Listen all pod, add route if needed
func (c *ConnectOptions) addRouteDynamic(ctx context.Context) (cache.SharedIndexInformer, cache.SharedIndexInformer, error) {
	podNs, svcNs, err := util.GetNsForListPodAndSvc(ctx, c.clientset, []string{v1.NamespaceAll, c.OriginNamespace})
	if err != nil {
		return nil, nil, err
	}

	conf := rest.CopyConfig(c.config)
	conf.QPS = 1
	conf.Burst = 2
	clientSet, err := kubernetes.NewForConfig(conf)
	if err != nil {
		plog.G(ctx).Errorf("Failed to create clientset: %v", err)
		return nil, nil, err
	}
	svcIndexers := cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}
	svcInformer := informerv1.NewServiceInformer(clientSet, svcNs, 0, svcIndexers)
	svcTicker := time.NewTicker(time.Second * 15)
	_, err = svcInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			svcTicker.Reset(time.Second * 3)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			svcTicker.Reset(time.Second * 3)
		},
		DeleteFunc: func(obj interface{}) {
			svcTicker.Reset(time.Second * 3)
		},
	})
	if err != nil {
		plog.G(ctx).Errorf("Failed to add service event handler: %v", err)
		return nil, nil, err
	}

	go svcInformer.Run(ctx.Done())
	go func() {
		defer svcTicker.Stop()
		for ; ctx.Err() == nil; <-svcTicker.C {
			svcTicker.Reset(time.Second * 15)
			serviceList := svcInformer.GetIndexer().List()
			var ips = sets.New[string]()
			for _, service := range serviceList {
				svc, ok := service.(*v1.Service)
				if !ok {
					continue
				}
				ips.Insert(svc.Spec.ClusterIP)
				ips.Insert(svc.Spec.ClusterIPs...)
			}
			if ctx.Err() != nil {
				return
			}
			if ips.Len() == 0 {
				continue
			}
			err := c.addRoute(ips.UnsortedList()...)
			if err != nil {
				plog.G(ctx).Debugf("Add service IP to route table failed: %v", err)
			}
		}
	}()

	podIndexers := cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}
	podInformer := informerv1.NewPodInformer(clientSet, podNs, 0, podIndexers)
	podTicker := time.NewTicker(time.Second * 15)
	_, err = podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			podTicker.Reset(time.Second * 3)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			podTicker.Reset(time.Second * 3)
		},
		DeleteFunc: func(obj interface{}) {
			podTicker.Reset(time.Second * 3)
		},
	})
	if err != nil {
		plog.G(ctx).Errorf("Failed to add service event handler: %v", err)
		return nil, nil, err
	}
	go podInformer.Run(ctx.Done())
	go func() {
		defer podTicker.Stop()
		for ; ctx.Err() == nil; <-podTicker.C {
			podTicker.Reset(time.Second * 15)
			podList := podInformer.GetIndexer().List()
			var ips = sets.New[string]()
			for _, pod := range podList {
				p, ok := pod.(*v1.Pod)
				if !ok {
					continue
				}
				if p.Spec.HostNetwork {
					continue
				}
				ips.Insert(util.GetPodIP(*p)...)
			}
			if ctx.Err() != nil {
				return
			}
			if ips.Len() == 0 {
				continue
			}
			err := c.addRoute(ips.UnsortedList()...)
			if err != nil {
				plog.G(ctx).Debugf("Add pod IP to route table failed: %v", err)
			}
		}
	}()

	return svcInformer, podInformer, nil
}

func (c *ConnectOptions) addRoute(ipStrList ...string) error {
	if c.tunName == "" {
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
		if r != nil {
			ifi, _, _, err := r.Route(ip)
			if err == nil && ifi.Name == c.tunName {
				continue
			}
		}
		routes = append(routes, types.Route{Dst: net.IPNet{IP: ip, Mask: mask}})
	}
	if len(routes) == 0 {
		return nil
	}
	err := tun.AddRoutes(c.tunName, routes...)
	return err
}

func (c *ConnectOptions) setupDNS(ctx context.Context, svcInformer cache.SharedIndexInformer) error {
	const portTCP = 10801
	podList, err := c.GetRunningPodList(ctx)
	if err != nil {
		plog.G(ctx).Errorf("Get running pod list failed, err: %v", err)
		return err
	}
	pod := podList[0]
	plog.G(ctx).Infof("Get DNS service IP from Pod...")
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

	plog.G(ctx).Infof("Adding extra domain to hosts...")
	if err = c.addExtraRoute(c.ctx, pod.GetName()); err != nil {
		plog.G(ctx).Errorf("Add extra route failed: %v", err)
		return err
	}

	ns := []string{c.OriginNamespace}
	list, err := c.clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: 100})
	if err == nil {
		for _, item := range list.Items {
			if !sets.New[string](ns...).Has(item.Name) {
				ns = append(ns, item.Name)
			}
		}
	}

	plog.G(ctx).Infof("Listing namespace %s services...", c.OriginNamespace)
	c.dnsConfig = &dns.Config{
		Config:      relovConf,
		Ns:          ns,
		Services:    []v1.Service{},
		SvcInformer: svcInformer,
		TunName:     c.tunName,
		Hosts:       c.extraHost,
		Lock:        c.Lock,
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
	plog.G(ctx).Infof("Setup DNS server for device %s...", c.tunName)
	if err = c.dnsConfig.SetupDNS(ctx); err != nil {
		return err
	}
	plog.G(ctx).Infof("Dump service in namespace %s into hosts...", c.OriginNamespace)
	// dump service in current namespace for support DNS resolve service:port
	err = c.dnsConfig.AddServiceNameToHosts(ctx, c.extraHost...)
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
		return nil, fmt.Errorf("server is empty, server config: %s", strings.Join(r.Listeners, ","))
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
func (c *ConnectOptions) getCIDR(ctx context.Context, filterAPIServer bool) error {
	var err error
	if filterAPIServer {
		c.apiServerIPs, err = util.GetAPIServerIP(c.config.Host)
		if err != nil {
			return err
		}
	}

	// (1) get CIDR from cache
	var ipPoolStr string
	ipPoolStr, err = c.Get(ctx, config.KeyClusterIPv4POOLS)
	if err != nil {
		return err
	}
	if strings.TrimSpace(ipPoolStr) != "" {
		for _, s := range strings.Split(ipPoolStr, " ") {
			_, cidr, _ := net.ParseCIDR(s)
			if cidr != nil {
				c.cidrs = util.RemoveCIDRsContainingIPs(util.RemoveLargerOverlappingCIDRs(append(c.cidrs, cidr)), c.apiServerIPs)
			}
		}
		plog.G(ctx).Infoln("Get network CIDR from cache")
		return nil
	}

	// (2) get CIDR from cni
	cidrs := util.GetCIDR(ctx, c.clientset, c.config, c.Namespace, c.Image)
	c.cidrs = util.RemoveCIDRsContainingIPs(util.RemoveLargerOverlappingCIDRs(cidrs), c.apiServerIPs)
	s := sets.New[string]()
	for _, cidr := range c.cidrs {
		s.Insert(cidr.String())
	}
	return c.Set(ctx, config.KeyClusterIPv4POOLS, strings.Join(s.UnsortedList(), " "))
}

func (c *ConnectOptions) Set(ctx context.Context, key, value string) error {
	err := retry.RetryOnConflict(
		retry.DefaultRetry,
		func() error {
			p := []byte(fmt.Sprintf(`[{"op": "replace", "path": "/data/%s", "value": "%s"}]`, key, value))
			_, err := c.clientset.CoreV1().ConfigMaps(c.Namespace).Patch(ctx, config.ConfigMapPodTrafficManager, k8stypes.JSONPatchType, p, metav1.PatchOptions{})
			return err
		})
	if err != nil {
		plog.G(ctx).Errorf("Failed to update configmap: %v", err)
		return err
	}
	return nil
}

func (c *ConnectOptions) Get(ctx context.Context, key string) (string, error) {
	cm, err := c.clientset.CoreV1().ConfigMaps(c.Namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return cm.Data[key], nil
}

func (c *ConnectOptions) addExtraRoute(ctx context.Context, name string) error {
	if len(c.ExtraRouteInfo.ExtraDomain) == 0 {
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
	for _, domain := range c.ExtraRouteInfo.ExtraDomain {
		output, err := util.Shell(ctx, c.clientset, c.config, name, config.ContainerSidecarVPN, c.Namespace, []string{"dig", "+short", domain})
		if err != nil {
			return errors.WithMessage(err, "failed to resolve DNS for domain by command dig")
		}
		var ip string
		if parseIP(output) == nil {
			// try to get ingress record
			ip = getIngressRecord(ctx, c.clientset.NetworkingV1(), []string{v1.NamespaceAll, c.Namespace}, domain)
		} else {
			ip = parseIP(output).String()
		}
		if net.ParseIP(ip) == nil {
			return fmt.Errorf("failed to resolve DNS for domain %s by command dig, output: %s", domain, output)
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
	if c.LocalTunIPv4 != nil {
		v4 = c.LocalTunIPv4.IP.String()
	}
	if c.LocalTunIPv6 != nil {
		v6 = c.LocalTunIPv6.IP.String()
	}
	return
}

func (c *ConnectOptions) GetConnectionID() string {
	if c != nil && c.dhcp != nil {
		return c.dhcp.GetConnectionID()
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
	// check running pod, sometime deployment is rolling back, so need to check running pod
	podList, err := c.GetRunningPodList(ctx)
	if err != nil {
		return err
	}

	clientVer := config.Version
	clientImg := c.Image
	serverImg := deploy.Spec.Template.Spec.Containers[0].Image
	runningPodImg := podList[0].Spec.Containers[0].Image

	isNeedUpgrade, err := util.IsNewer(clientVer, clientImg, serverImg)
	isPodNeedUpgrade, err1 := util.IsNewer(clientVer, clientImg, runningPodImg)
	if !isNeedUpgrade && !isPodNeedUpgrade {
		return nil
	}
	if err != nil {
		return err
	}
	if err1 != nil {
		return err1
	}

	// 1) update secret
	err = upgradeSecretSpec(ctx, c.factory, c.Namespace)
	if err != nil {
		return err
	}

	// 2) update deploy
	plog.G(ctx).Infof("Set image %s --> %s...", serverImg, clientImg)
	err = upgradeDeploySpec(ctx, c.factory, c.Namespace, deploy.Name, clientImg)
	if err != nil {
		return err
	}
	// 3) update service
	err = upgradeServiceSpec(ctx, c.factory, c.Namespace)
	if err != nil {
		return err
	}
	return nil
}

func upgradeDeploySpec(ctx context.Context, f cmdutil.Factory, ns, name, image string) error {
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
			tcp10801 := "10801-for-tcp"
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
			deploySpec := genDeploySpec(ns, tcp10801, tcp9002, udp53, tcp80, image, imagePullSecret)
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

func upgradeSecretSpec(ctx context.Context, f cmdutil.Factory, ns string) error {
	crt, key, host, err := util.GenTLSCert(ctx, ns)
	if err != nil {
		return err
	}
	secret := genSecret(ns, crt, key, host)

	clientset, err := f.KubernetesClientSet()
	if err != nil {
		return err
	}
	currentSecret, err := clientset.CoreV1().Secrets(ns).Get(ctx, secret.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	// already have three keys
	if currentSecret.Data[config.TLSServerName] != nil &&
		currentSecret.Data[config.TLSPrivateKeyKey] != nil &&
		currentSecret.Data[config.TLSCertKey] != nil {
		return nil
	}

	_, err = clientset.CoreV1().Secrets(ns).Update(ctx, secret, metav1.UpdateOptions{})
	if err != nil {
		return err
	}

	mutatingWebhookConfig := genMutatingWebhookConfiguration(ns, crt)
	var current *admissionv1.MutatingWebhookConfiguration
	current, err = clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx, mutatingWebhookConfig.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	mutatingWebhookConfig.ResourceVersion = current.ResourceVersion
	_, err = clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Update(ctx, mutatingWebhookConfig, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	return nil
}

func upgradeServiceSpec(ctx context.Context, f cmdutil.Factory, ns string) error {
	tcp10801 := "10801-for-tcp"
	tcp9002 := "9002-for-envoy"
	tcp80 := "80-for-webhook"
	udp53 := "53-for-dns"
	svcSpec := genService(ns, tcp10801, tcp9002, tcp80, udp53)

	clientset, err := f.KubernetesClientSet()
	if err != nil {
		return err
	}
	currentSecret, err := clientset.CoreV1().Services(ns).Get(ctx, svcSpec.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	svcSpec.ResourceVersion = currentSecret.ResourceVersion

	_, err = clientset.CoreV1().Services(ns).Update(ctx, svcSpec, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	return nil
}

func (c *ConnectOptions) GetTunDeviceName() (string, error) {
	var ips []net.IP
	if c.LocalTunIPv4 != nil {
		ips = append(ips, c.LocalTunIPv4.IP)
	}
	if c.LocalTunIPv6 != nil {
		ips = append(ips, c.LocalTunIPv6.IP)
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

func healthCheckPortForward(ctx context.Context, cancelFunc context.CancelFunc, readyChan chan struct{}, localGvisorUDPPort string, domain string, ipv4 net.IP) {
	defer cancelFunc()
	ticker := time.NewTicker(time.Second * 60)
	defer ticker.Stop()

	select {
	case <-readyChan:
	case <-ticker.C:
		plog.G(ctx).Debugf("Wait port-forward to be ready timeout")
		return
	case <-ctx.Done():
		return
	}

	var healthChecker = func() error {
		conn, err := net.Dial("tcp", fmt.Sprintf(":%s", localGvisorUDPPort))
		if err != nil {
			return err
		}
		defer conn.Close()
		err = util.WriteProxyInfo(conn, stack.TransportEndpointID{
			LocalPort:     53,
			LocalAddress:  tcpip.AddrFrom4Slice(net.ParseIP("127.0.0.1").To4()),
			RemotePort:    0,
			RemoteAddress: tcpip.AddrFrom4Slice(ipv4.To4()),
		})
		if err != nil {
			return err
		}

		packetConn, _ := core.NewPacketConnOverTCP(ctx, conn)
		defer packetConn.Close()

		msg := new(miekgdns.Msg)
		msg.SetQuestion(miekgdns.Fqdn(domain), miekgdns.TypeA)
		client := miekgdns.Client{Net: "udp", Timeout: time.Second * 10}
		_, _, err = client.ExchangeWithConnContext(ctx, msg, &miekgdns.Conn{Conn: packetConn})
		return err
	}

	newTicker := time.NewTicker(time.Second * 10)
	defer newTicker.Stop()
	for ; ctx.Err() == nil; <-newTicker.C {
		err := retry.OnError(wait.Backoff{Duration: time.Second * 5, Steps: 4}, func(err error) bool {
			return err != nil
		}, func() error {
			return healthChecker()
		})
		if err != nil {
			plog.G(ctx).Errorf("Failed to query DNS: %v", err)
			return
		}
	}
}

func healthCheckTCPConn(ctx context.Context, cancelFunc context.CancelFunc, readyChan chan struct{}, domain string, dnsServer string) {
	defer cancelFunc()
	ticker := time.NewTicker(time.Second * 60)
	defer ticker.Stop()

	select {
	case <-readyChan:
	case <-ticker.C:
		plog.G(ctx).Debugf("Wait port-forward to be ready timeout")
		return
	case <-ctx.Done():
		return
	}

	var healthChecker = func() error {
		msg := new(miekgdns.Msg)
		msg.SetQuestion(miekgdns.Fqdn(domain), miekgdns.TypeA)
		client := miekgdns.Client{Net: "udp", Timeout: time.Second * 10}
		_, _, err := client.ExchangeContext(ctx, msg, net.JoinHostPort(dnsServer, "53"))
		return err
	}

	newTicker := time.NewTicker(config.KeepAliveTime / 2)
	defer newTicker.Stop()
	for ; ctx.Err() == nil; <-newTicker.C {
		err := retry.OnError(wait.Backoff{Duration: time.Second * 10, Steps: 6}, func(err error) bool {
			return err != nil
		}, func() error {
			return healthChecker()
		})
		if err != nil {
			plog.G(ctx).Errorf("Failed to query DNS: %v", err)
			return
		}
	}
}
