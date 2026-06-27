package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/metadata"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/cli-runtime/pkg/resource"
	informerv1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dhcp"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
	"github.com/wencaiwulue/kubevpn/v2/pkg/driver"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// ConnectOptions holds all state for a kubevpn connection session.
type ConnectOptions struct {
	ManagerNamespace     string
	ExtraRouteInfo       ExtraRouteInfo
	Foreground           bool
	OriginKubeconfigPath string
	OriginNamespace      string
	Lock                 *sync.Mutex
	Image                string
	ImagePullSecretName  string
	// for reload from ~/.kubevpn/daemon/db
	Request *rpc.ConnectRequest `json:"Request,omitempty"`

	ctx         context.Context
	cancel      context.CancelFunc
	isDataPlane bool

	clientset  kubernetes.Interface
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
	healthStatus     HealthStatus
	cmInformerOnce   sync.Once
	cmInformer       cache.SharedInformer
	cmInformerStop   chan struct{}

	Sync *SyncOptions
}

// Context returns the connection session's context.
func (c *ConnectOptions) Context() context.Context {
	return c.ctx
}

// InitDHCP initializes the DHCP manager for IP allocation if not already created.
func (c *ConnectOptions) InitDHCP(ctx context.Context) error {
	if c.dhcp == nil {
		c.dhcp = dhcp.NewDHCPManager(c.clientset, c.ManagerNamespace)
		return c.dhcp.InitDHCP(ctx)
	}
	return nil
}

// RentIP leases IPv4 and IPv6 TUN addresses from DHCP, or reuses the provided CIDRs if valid.
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

// GetIPFromContext extracts IPv4 and IPv6 TUN addresses from incoming gRPC metadata.
func (c *ConnectOptions) GetIPFromContext(ctx context.Context, logger *log.Logger) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return fmt.Errorf("cannot get IP from context")
	}

	ipv4 := md.Get(config.HeaderIPv4)
	if len(ipv4) == 0 {
		return fmt.Errorf("cannot find IPv4 from header: %v", md)
	}
	ip, ipNet, err := net.ParseCIDR(ipv4[0])
	if err != nil {
		return fmt.Errorf("cannot convert IPv4 string: %s: %w", ipv4[0], err)
	}
	c.LocalTunIPv4 = &net.IPNet{IP: ip, Mask: ipNet.Mask}
	logger.Debugf("Get IPv4 %s from context", c.LocalTunIPv4.String())

	ipv6 := md.Get(config.HeaderIPv6)
	if len(ipv6) == 0 {
		return fmt.Errorf("cannot find IPv6 from header: %v", md)
	}
	ip, ipNet, err = net.ParseCIDR(ipv6[0])
	if err != nil {
		return fmt.Errorf("cannot convert IPv6 string: %s: %w", ipv6[0], err)
	}
	c.LocalTunIPv6 = &net.IPNet{IP: ip, Mask: ipNet.Mask}
	logger.Debugf("Get IPv6 %s from context", c.LocalTunIPv6.String())
	return nil
}

// CreateRemoteInboundPod injects Envoy sidecar proxies into the specified workloads for inbound traffic interception.
func (c *ConnectOptions) CreateRemoteInboundPod(ctx context.Context, namespace string, workloads []string, headers map[string]string, portMap []string, image string) (err error) {
	if c.LocalTunIPv4 == nil || c.LocalTunIPv6 == nil {
		return fmt.Errorf("local tun IP is invalid")
	}
	if c.proxyWorkloads == nil {
		c.proxyWorkloads = make(ProxyList, 0)
	}

	tlsSecret, err := c.clientset.CoreV1().Secrets(c.ManagerNamespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return err
	}

	for _, workload := range workloads {
		plog.G(ctx).Infof("Injecting inbound sidecar for %s in namespace %s", workload, namespace)
		localTunIPv4 := c.LocalTunIPv4.IP.String()
		localTunIPv6 := c.LocalTunIPv6.IP.String()
		var object, controller *resource.Info
		object, controller, err = util.GetTopOwnerObject(ctx, c.factory, namespace, workload)
		if err != nil {
			return err
		}
		var templateSpec *v1.PodTemplateSpec
		templateSpec, _, err = util.GetPodTemplateSpecPath(controller.Object.(*unstructured.Unstructured))
		if err != nil {
			return err
		}
		var mapper *Mapper
		if util.IsK8sService(object) {
			mapper = NewMapper(c.clientset, namespace, labels.SelectorFromSet(templateSpec.Labels).String(), headers, workload, c.GetConfigMapInformer())
		}
		c.proxyWorkloads.Add(&Proxy{
			headers:    headers,
			portMap:    portMap,
			workload:   workload,
			namespace:  namespace,
			portMapper: mapper,
		})

		nodeID := fmt.Sprintf("%s.%s", object.Mapping.Resource.GroupResource().String(), object.Name)
		// todo consider to use ephemeral container
		// https://kubernetes.io/docs/concepts/workloads/pods/ephemeral-containers/
		injector := inject.NewInjector(inject.InjectOptions{
			Factory:          c.factory,
			Clientset:        c.clientset,
			ManagerNamespace: c.ManagerNamespace,
			NodeID:           nodeID,
			Object:           object,
			Controller:       controller,
			LocalTunIPv4:     localTunIPv4,
			LocalTunIPv6:     localTunIPv6,
			Headers:          headers,
			PortMaps:         portMap,
			Secret:           tlsSecret,
			Image:            image,
		})
		err = injector.Inject(ctx)
		if err != nil {
			plog.G(ctx).Errorf("Injecting inbound sidecar for %s in namespace %s failed: %v", workload, namespace, err)
			return err
		}
		plog.G(ctx).Infof("Injected inbound sidecar for %s in namespace %s successfully", workload, namespace)
		if mapper != nil {
			go mapper.Run()
		}
	}
	return nil
}

// DoConnect establishes the full VPN connection: DHCP, CIDR detection, port forwarding, TUN device, routing, and DNS.
func (c *ConnectOptions) DoConnect(ctx context.Context) (err error) {
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.isDataPlane = true
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
	if err = createOutboundPod(c.ctx, c.clientset, c.ManagerNamespace, c.Image, c.ImagePullSecretName); err != nil {
		return
	}
	if err = c.upgradeDeploy(c.ctx); err != nil {
		return
	}
	if err = c.addExtraNodeIP(c.ctx); err != nil {
		plog.G(ctx).Errorf("Add extra node IP failed: %v", err)
		return
	}
	var gvisorTCPForwardPort, gvisorUDPForwardPort int
	gvisorTCPForwardPort, err = util.GetAvailableTCPPort()
	if err != nil {
		return err
	}
	gvisorUDPForwardPort, err = util.GetAvailableTCPPort()
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

// InitClient initializes the Kubernetes clientset, REST client, and config from the given factory.
func (c *ConnectOptions) InitClient(f cmdutil.Factory) error {
	plog.G(context.Background()).Debug("Initializing Kubernetes client")
	c.factory = f
	var err error
	c.config, c.restclient, c.clientset, c.ManagerNamespace, err = util.InitKubeClient(f)
	return err
}

// GetRunningPodList returns the running traffic manager pods in the manager namespace.
func (c *ConnectOptions) GetRunningPodList(ctx context.Context) ([]v1.Pod, error) {
	label := "app=" + config.ConfigMapPodTrafficManager
	return util.GetRunningPodList(ctx, c.clientset, c.ManagerNamespace, label)
}

// getCIDR
// 1: get pod cidr
// 2: get service cidr
// distinguish service cidr and pod cidr
// https://stackoverflow.com/questions/45903123/kubernetes-set-service-cidr-and-pod-cidr-the-same
// https://stackoverflow.com/questions/44190607/how-do-you-find-the-cluster-service-cidr-of-a-kubernetes-cluster/54183373#54183373
// https://stackoverflow.com/questions/44190607/how-do-you-find-the-cluster-service-cidr-of-a-kubernetes-cluster
func (c *ConnectOptions) getCIDR(ctx context.Context, filterAPIServer bool) error {
	plog.G(ctx).Debug("Detecting cluster CIDRs")
	var err error
	if filterAPIServer {
		c.apiServerIPs, err = util.GetAPIServerIP(c.config.Host)
		if err != nil {
			return err
		}
		if c.Request != nil {
			c.apiServerIPs = append(c.apiServerIPs, ssh.ParseSshFromRPC(c.Request.SshJump).Host()...)
		}
	}

	// (1) get CIDR from cache
	var ipPoolStr string
	ipPoolStr, err = c.Get(ctx, config.KeyClusterIPv4POOLS)
	if err != nil {
		return err
	}
	if strings.TrimSpace(ipPoolStr) != "" {
		var cached []*net.IPNet
		for _, s := range strings.Split(ipPoolStr, " ") {
			_, cidr, _ := net.ParseCIDR(s)
			if cidr != nil {
				cached = append(cached, cidr)
			}
		}
		c.cidrs = util.RemoveCIDRsContainingIPs(util.RemoveLargerOverlappingCIDRs(cached), c.apiServerIPs)
		plog.G(ctx).Infof("Get network CIDR from cache")
		return nil
	}

	// (2) get CIDR from cni
	cidrs := util.GetCIDR(ctx, c.clientset, c.config, c.ManagerNamespace, c.Image)
	c.cidrs = util.RemoveCIDRsContainingIPs(util.RemoveLargerOverlappingCIDRs(cidrs), c.apiServerIPs)
	s := sets.New[string]()
	for _, cidr := range c.cidrs {
		s.Insert(cidr.String())
	}
	return c.Set(ctx, config.KeyClusterIPv4POOLS, strings.Join(s.UnsortedList(), " "))
}

// Set updates a key-value pair in the traffic manager ConfigMap.
func (c *ConnectOptions) Set(ctx context.Context, key, value string) error {
	err := retry.RetryOnConflict(
		retry.DefaultRetry,
		func() error {
			patch := []map[string]string{{
				"op":    "replace",
				"path":  "/data/" + key,
				"value": value,
			}}
			p, err := json.Marshal(patch)
			if err != nil {
				return fmt.Errorf("failed to marshal JSON patch: %w", err)
			}
			_, err = c.clientset.CoreV1().ConfigMaps(c.ManagerNamespace).Patch(ctx, config.ConfigMapPodTrafficManager, k8stypes.JSONPatchType, p, metav1.PatchOptions{})
			return err
		})
	if err != nil {
		plog.G(ctx).Errorf("Failed to update configmap: %v", err)
		return err
	}
	return nil
}

// Get retrieves a value by key from the traffic manager ConfigMap, using the informer cache first.
func (c *ConnectOptions) Get(ctx context.Context, key string) (string, error) {
	items := c.GetConfigMapInformer().GetStore().List()
	for _, item := range items {
		if cm, ok := item.(*v1.ConfigMap); ok {
			return cm.Data[key], nil
		}
	}
	cm, err := c.clientset.CoreV1().ConfigMaps(c.ManagerNamespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return cm.Data[key], nil
}

// GetConfigMapInformer returns a shared informer for the traffic manager ConfigMap.
// Created once on first call, then reused. Thread-safe via sync.Once.
// Must be called after InitClient.
func (c *ConnectOptions) GetConfigMapInformer() cache.SharedInformer {
	c.cmInformerOnce.Do(func() {
		c.cmInformer = informerv1.NewFilteredConfigMapInformer(
			c.clientset, c.ManagerNamespace, 0, cache.Indexers{},
			func(options *metav1.ListOptions) {
				options.FieldSelector = fields.OneTermEqualSelector("metadata.name", config.ConfigMapPodTrafficManager).String()
			},
		)
		c.cmInformerStop = make(chan struct{})
		go c.cmInformer.Run(c.cmInformerStop)
	})
	return c.cmInformer
}

// GetClientset returns the Kubernetes clientset for this connection.
func (c *ConnectOptions) GetClientset() kubernetes.Interface {
	return c.clientset
}

// GetFactory returns the kubectl factory for this connection.
func (c *ConnectOptions) GetFactory() cmdutil.Factory {
	return c.factory
}

// GetLocalTunIP returns the local TUN device IPv4 and IPv6 addresses as strings.
func (c *ConnectOptions) GetLocalTunIP() (v4 string, v6 string) {
	if c.LocalTunIPv4 != nil {
		v4 = c.LocalTunIPv4.IP.String()
	}
	if c.LocalTunIPv6 != nil {
		v6 = c.LocalTunIPv6.IP.String()
	}
	return
}

// GetConnectionID returns the DHCP connection identifier for this session.
func (c *ConnectOptions) GetConnectionID() string {
	if c != nil && c.dhcp != nil {
		return c.dhcp.GetConnectionID()
	}
	return ""
}

// GetTunDeviceName returns the OS network interface name of the TUN device for this connection.
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

// AddRollbackFunc registers a cleanup function to be called when the connection is torn down.
func (c *ConnectOptions) AddRollbackFunc(f func() error) {
	c.rollbackFuncList = append(c.rollbackFuncList, f)
}

func (c *ConnectOptions) getRollbackFuncs() []func() error {
	return c.rollbackFuncList
}

func (c *ConnectOptions) leavePortMap(ns, workload string) {
	c.proxyWorkloads.Remove(ns, workload)
}

// IsMe reports whether this connection owns the proxy for the given namespace, UID, and headers.
func (c *ConnectOptions) IsMe(ns, uid string, headers map[string]string) bool {
	return c.proxyWorkloads.IsMe(ns, uid, headers)
}

// ProxyResources returns the list of workloads currently being proxied by this connection.
func (c *ConnectOptions) ProxyResources() ProxyList {
	return c.proxyWorkloads
}
