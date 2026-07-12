package handler

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/tools/cache"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// ConnectOptions holds all state for a kubevpn connection session.
type ConnectOptions struct {
	K8sClient

	ManagerNamespace     string
	ExtraRouteInfo       ExtraRouteInfo
	OriginKubeconfigPath string
	WorkloadNamespace    string
	Lock                 *sync.Mutex
	Image                string
	ImagePullSecretName  string
	// RequestRaw stores the protobuf-serialized ConnectRequest for daemon restart replay.
	RequestRaw []byte `json:"RequestRaw,omitempty"`

	ctx          context.Context
	cancel       context.CancelFunc
	isDataPlane  bool
	OwnerID      string `json:"OwnerID,omitempty"`
	ConnectionID string

	rollbackMu       sync.Mutex
	rollbackFuncList []func() error

	SshHosts       []net.IP `json:"-"`
	cleanupMu      sync.Mutex
	cleanedUp      bool
	network        *NetworkManager
	proxyManager   *ProxyManager
	configMapStore *ConfigMapStore

	Sync *SyncOptions
}

// Context returns the connection session's context.
func (c *ConnectOptions) Context() context.Context {
	return c.ctx
}

// CreateRemoteInboundPod injects Envoy sidecar proxies into the specified workloads for inbound traffic interception.
func (c *ConnectOptions) CreateRemoteInboundPod(ctx context.Context, namespace string, workloads []string, headers map[string]string, portMap []string, image string, localTunIPv4, localTunIPv6 string) (err error) {
	if localTunIPv4 == "" {
		return fmt.Errorf("local tun IPv4 is empty")
	}
	if c.proxyManager == nil {
		c.proxyManager = newProxyManager(c.factory, c.clientset, c.ManagerNamespace)
	}

	tlsSecret, err := c.clientset.CoreV1().Secrets(c.ManagerNamespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return err
	}

	for _, workload := range workloads {
		plog.G(ctx).Infof("Injecting inbound sidecar for %s in namespace %s", workload, namespace)
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
		c.proxyManager.Add(&Proxy{
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
			OwnerID:          c.OwnerID,
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

// CreateOutboundPod ensures the traffic manager pod exists and is ready.
// This is a control-plane responsibility and should be called from the user daemon
// before calling the sudo daemon, so the TunConfigService is available for IP allocation.
func (c *ConnectOptions) CreateOutboundPod(ctx context.Context) error {
	return createOutboundPod(ctx, c.clientset, c.ManagerNamespace, c.Image, c.ImagePullSecretName)
}

// DoConnect establishes the full VPN connection: CIDR detection, port forwarding, TUN device, routing, and DNS.
// Control-plane setup (CreateOutboundPod, UpgradeDeploy) must be done before this call.
func (c *ConnectOptions) DoConnect(ctx context.Context) (err error) {
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.isDataPlane = true
	plog.G(ctx).Info("Starting connect to cluster")
	go c.setupSignalHandler()
	var cidrs []*net.IPNet
	var apiServerIPs []net.IP
	if cidrs, apiServerIPs, err = c.getCIDR(c.ctx); err != nil {
		plog.G(ctx).Errorf("Failed to get network CIDR: %v", err)
		return
	}

	c.network = newNetworkManager(NetworkConfig{
		Clientset:         c.clientset,
		RESTClient:        c.restclient,
		Config:            c.config,
		ManagerNamespace:  c.ManagerNamespace,
		WorkloadNamespace: c.WorkloadNamespace,
		CIDRs:             cidrs,
		APIServerIPs:      apiServerIPs,
		ExtraRouteInfo:    &c.ExtraRouteInfo,
		Image:             c.Image,
		Lock:              c.Lock,
		OwnerID:           c.OwnerID,
		GetRunningPodList: c.GetRunningPodList,
	})
	if err = c.network.Start(c.ctx); err != nil {
		return
	}
	c.network.StartIPWatcher(c.ctx)
	return
}

// InitClient initializes the Kubernetes clientset, REST client, and config from the given factory.
func (c *ConnectOptions) InitClient(f cmdutil.Factory) error {
	var err error
	c.ManagerNamespace, err = c.K8sClient.InitClient(f)
	return err
}

// getConfigMapStore returns the ConfigMapStore, creating it lazily on first access.
// This must be lazy because ManagerNamespace may be updated by detectAndSetManagerNamespace
// after InitClient returns (user daemon path).
func (c *ConnectOptions) getConfigMapStore() *ConfigMapStore {
	if c.configMapStore == nil {
		c.configMapStore = newConfigMapStore(c.clientset, c.ManagerNamespace)
	}
	return c.configMapStore
}

// GetRunningPodList returns the running traffic manager pods in the manager namespace.
func (c *ConnectOptions) GetRunningPodList(ctx context.Context) ([]v1.Pod, error) {
	label := "app=" + config.ConfigMapPodTrafficManager
	return util.GetRunningPodList(ctx, c.clientset, c.ManagerNamespace, label)
}

func dedupAndFilterCIDRs(cidrs []*net.IPNet, apiServerIPs []net.IP) []*net.IPNet {
	return util.RemoveCIDRsContainingIPs(util.RemoveLargerOverlappingCIDRs(cidrs), apiServerIPs)
}

// parseCachedCIDRs parses a space-separated CIDR string (from ConfigMap cache)
// and returns the parsed CIDRs after dedup and API server IP filtering.
func parseCachedCIDRs(ipPoolStr string, apiServerIPs []net.IP) []*net.IPNet {
	var cidrs []*net.IPNet
	for _, s := range strings.Split(ipPoolStr, " ") {
		_, cidr, _ := net.ParseCIDR(s)
		if cidr != nil {
			cidrs = append(cidrs, cidr)
		}
	}
	return dedupAndFilterCIDRs(cidrs, apiServerIPs)
}

// encodeCIDRs serializes CIDRs into a deduplicated space-separated string for ConfigMap storage.
func encodeCIDRs(cidrs []*net.IPNet) string {
	s := sets.New[string]()
	for _, cidr := range cidrs {
		s.Insert(cidr.String())
	}
	return strings.Join(s.UnsortedList(), " ")
}

// getAPIServerIPs resolves the API server IPs from the kubeconfig host, appending SSH jump host IPs.
func (c *ConnectOptions) getAPIServerIPs() ([]net.IP, error) {
	ips, err := util.GetAPIServerIP(c.config.Host)
	if err != nil {
		return nil, err
	}
	return append(ips, c.SshHosts...), nil
}

// getCIDR detects cluster Pod/Service CIDRs and API server IPs.
func (c *ConnectOptions) getCIDR(ctx context.Context) ([]*net.IPNet, []net.IP, error) {
	plog.G(ctx).Debug("Detecting cluster CIDRs")
	apiServerIPs, err := c.getAPIServerIPs()
	if err != nil {
		return nil, nil, err
	}

	ipPoolStr, err := c.Get(ctx, config.KeyClusterCIDRs)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(ipPoolStr) != "" {
		cidrs := parseCachedCIDRs(ipPoolStr, apiServerIPs)
		plog.G(ctx).Infof("Get network CIDR from cache")
		return cidrs, apiServerIPs, nil
	}

	raw := util.GetCIDR(ctx, c.clientset, c.config, c.ManagerNamespace, c.Image)
	cidrs := dedupAndFilterCIDRs(raw, apiServerIPs)
	if len(cidrs) == 0 {
		plog.G(ctx).Debugf("No cluster CIDRs detected (raw=%d, all filtered by API server IPs %v)", len(raw), apiServerIPs)
		return cidrs, apiServerIPs, nil
	}
	encoded := encodeCIDRs(cidrs)
	plog.G(ctx).Debugf("Saving %d cluster CIDRs to cache: %s", len(cidrs), encoded)
	err = c.Set(ctx, config.KeyClusterCIDRs, encoded)
	return cidrs, apiServerIPs, err
}

// Set updates a key-value pair in the traffic manager ConfigMap.
func (c *ConnectOptions) Set(ctx context.Context, key, value string) error {
	return c.getConfigMapStore().Set(ctx, key, value)
}

// Get retrieves a value by key from the traffic manager ConfigMap, using the informer cache first.
func (c *ConnectOptions) Get(ctx context.Context, key string) (string, error) {
	return c.getConfigMapStore().Get(ctx, key)
}

// GetConfigMapInformer returns a shared informer for the traffic manager ConfigMap.
// Created once on first call, then reused. Thread-safe via sync.Once.
// Must be called after InitClient.
func (c *ConnectOptions) GetConfigMapInformer() cache.SharedInformer {
	return c.getConfigMapStore().GetInformer()
}

// HealthCheckOnce performs a single health check with the given timeout.
func (c *ConnectOptions) HealthCheckOnce(ctx context.Context, timeout time.Duration) {
	c.getConfigMapStore().HealthCheckOnce(ctx, timeout)
}

// HealthPeriod periodically syncs health status on the given interval.
func (c *ConnectOptions) HealthPeriod(ctx context.Context, interval time.Duration) {
	c.getConfigMapStore().HealthPeriod(ctx, interval)
}

// HealthStatus returns the last known health state.
func (c *ConnectOptions) HealthStatus() HealthStatus {
	return c.getConfigMapStore().GetHealthStatus()
}

// GetLocalTunIP returns the local TUN device IPv4 and IPv6 addresses as strings.
// Only meaningful in the data-plane (sudo daemon) where NetworkManager holds the allocated IP.
func (c *ConnectOptions) GetLocalTunIP() (v4 string, v6 string) {
	if c.network != nil {
		if ip := c.network.LocalTunIPv4(); ip != nil {
			v4 = ip.IP.String()
		}
		if ip := c.network.LocalTunIPv6(); ip != nil {
			v6 = ip.IP.String()
		}
	}
	return
}

// GetConnectionID returns the connection identifier (namespace UID suffix) for this session.
func (c *ConnectOptions) GetConnectionID() string {
	if c == nil {
		return ""
	}
	return c.ConnectionID
}

// AddRollbackFunc registers a cleanup function to be called when the connection is torn down.
func (c *ConnectOptions) AddRollbackFunc(f func() error) {
	c.rollbackMu.Lock()
	defer c.rollbackMu.Unlock()
	c.rollbackFuncList = append(c.rollbackFuncList, f)
}

func (c *ConnectOptions) getRollbackFuncs() []func() error {
	c.rollbackMu.Lock()
	defer c.rollbackMu.Unlock()
	fns := make([]func() error, len(c.rollbackFuncList))
	copy(fns, c.rollbackFuncList)
	return fns
}

// ProxyResources returns the list of workloads currently being proxied by this connection.
func (c *ConnectOptions) ProxyResources() ProxyList {
	if c.proxyManager == nil {
		return nil
	}
	return c.proxyManager.Resources()
}

// GetManagerNamespace returns the namespace where the traffic manager is deployed.
func (c *ConnectOptions) GetManagerNamespace() string {
	return c.ManagerNamespace
}

// GetOriginKubeconfigPath returns the original kubeconfig file path.
func (c *ConnectOptions) GetOriginKubeconfigPath() string {
	return c.OriginKubeconfigPath
}

// GetSync returns the SyncOptions associated with this connection, or nil.
func (c *ConnectOptions) GetSync() *SyncOptions {
	return c.Sync
}
