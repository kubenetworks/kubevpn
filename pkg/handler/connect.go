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
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// ConnectOptions holds all state for a user-daemon (control-plane) kubevpn connection session.
// It is the type stored in svr.connections when IsSudo=false.
//
// Type alias ControlSession = ConnectOptions is provided for semantic clarity.
// All existing code that uses ConnectOptions continues to compile unchanged.
type ConnectOptions struct {
	SessionBase

	// Persisted for daemon restart replay (json tag required).
	RequestRaw []byte `json:"RequestRaw,omitempty"`

	// Identity: set from the incoming ConnectRequest and persisted (json tags).
	ManagerNamespace     string
	WorkloadNamespace    string
	ExtraRouteInfo       ExtraRouteInfo
	OriginKubeconfigPath string
	Image                string
	ImagePullSecretName  string
	OwnerID              string `json:"OwnerID,omitempty"`
	ConnectionID         string

	// Control-plane only: SSH jump host IPs (added to API server IPs at CIDR detection).
	SshHosts []net.IP `json:"-"`

	// Control-plane only: sidecar injection lifecycle.
	proxyManager *ProxyManager

	// Control-plane only: file sync.
	syncMu sync.RWMutex
	Sync   *SyncOptions
}

// InitClient initializes the Kubernetes clientset, REST client, and config from the given factory.
func (c *ConnectOptions) InitClient(f cmdutil.Factory) error {
	var err error
	c.ManagerNamespace, err = c.K8sClient.InitClient(f)
	return err
}

// Context is a stub on the control-plane session (ConnectOptions/ControlSession).
// The data-plane context lives in DataSession; this returns nil for all user-daemon instances.
func (c *ConnectOptions) Context() context.Context {
	return nil
}

// DoConnect is not available on the control-plane session (ConnectOptions/ControlSession).
// It exists only to satisfy the Connection interface. The data-plane DoConnect
// runs on DataSession in the root daemon.
func (c *ConnectOptions) DoConnect(_ context.Context) error {
	return fmt.Errorf("DoConnect is not available on a control-plane session")
}

// GetLocalTunIP is a stub on the control-plane session.
// TUN IP allocation is performed by DataSession (root daemon).
func (c *ConnectOptions) GetLocalTunIP() (v4 string, v6 string) {
	return "", ""
}

// GetLastHeartbeat is a stub on the control-plane session.
// The user daemon obtains heartbeat info via the sudo Status RPC, not from a local NetworkManager.
func (c *ConnectOptions) GetLastHeartbeat() time.Time {
	return time.Time{}
}

// GetAPIServerIPs is a stub on the control-plane session.
// API server IPs are held by DataSession's NetworkManager in the root daemon.
func (c *ConnectOptions) GetAPIServerIPs() []net.IP {
	return nil
}

// GetNetworkExtraHost is a stub on the control-plane session.
// Extra hosts are accumulated by DataSession's NetworkManager in the root daemon.
func (c *ConnectOptions) GetNetworkExtraHost() []dns.Entry {
	return nil
}

// GetConnectionID returns the connection identifier (namespace UID suffix) for this session.
func (c *ConnectOptions) GetConnectionID() string {
	if c == nil {
		return ""
	}
	return c.ConnectionID
}

// GetManagerNamespace returns the namespace where the traffic manager is deployed.
func (c *ConnectOptions) GetManagerNamespace() string {
	return c.ManagerNamespace
}

// GetWorkloadNamespace returns the user workload namespace for this connection.
func (c *ConnectOptions) GetWorkloadNamespace() string {
	return c.WorkloadNamespace
}

// GetOwnerID returns the OwnerID field.
func (c *ConnectOptions) GetOwnerID() string {
	if c == nil {
		return ""
	}
	return c.OwnerID
}

// GetOriginKubeconfigPath returns the original kubeconfig file path.
func (c *ConnectOptions) GetOriginKubeconfigPath() string {
	return c.OriginKubeconfigPath
}

// GetExtraCIDR returns the extra CIDR strings configured for this connection.
func (c *ConnectOptions) GetExtraCIDR() []string {
	return c.ExtraRouteInfo.ExtraCIDR
}

// GetRunningPodList returns the running traffic manager pods in the manager namespace.
func (c *ConnectOptions) GetRunningPodList(ctx context.Context) ([]v1.Pod, error) {
	return c.SessionBase.GetRunningPodList(ctx, c.ManagerNamespace)
}

// getConfigMapStore returns the ConfigMapStore, creating it lazily on first access.
// This must be lazy because ManagerNamespace may be updated by detectAndSetManagerNamespace
// after InitClient returns (user daemon path).
func (c *ConnectOptions) getConfigMapStore() *ConfigMapStore {
	return c.SessionBase.getConfigMapStore(c.ManagerNamespace)
}

// Set updates a key-value pair in the traffic manager ConfigMap.
func (c *ConnectOptions) Set(ctx context.Context, key, value string) error {
	return c.getConfigMapStore().Set(ctx, key, value)
}

// Get retrieves a value by key from the traffic manager ConfigMap, using the informer cache first.
func (c *ConnectOptions) Get(ctx context.Context, key string) (string, error) {
	return c.getConfigMapStore().Get(ctx, key)
}

// GetTrafficManagerConfigMap returns the traffic manager ConfigMap from the informer cache
// (GET fallback when cold). Use this for read paths that want near-real-time state.
func (c *ConnectOptions) GetTrafficManagerConfigMap(ctx context.Context) (*v1.ConfigMap, error) {
	return c.getConfigMapStore().GetConfigMap(ctx)
}

// GetConfigMapInformer returns a shared informer for the traffic manager ConfigMap.
// Created once on first call, then reused. Thread-safe via sync.Once.
// Must be called after InitClient.
func (c *ConnectOptions) GetConfigMapInformer() cache.SharedInformer {
	return c.getConfigMapStore().GetInformer()
}

// ProxyResources returns the list of workloads currently being proxied by this connection.
func (c *ConnectOptions) ProxyResources() ProxyList {
	if c.proxyManager == nil {
		return nil
	}
	return c.proxyManager.Resources()
}

// GetSync returns the SyncOptions associated with this connection, or nil.
func (c *ConnectOptions) GetSync() *SyncOptions {
	c.syncMu.RLock()
	defer c.syncMu.RUnlock()
	return c.Sync
}

// SetSync stores the SyncOptions associated with this connection.
func (c *ConnectOptions) SetSync(s *SyncOptions) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	c.Sync = s
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

	plog.StepStart(ctx, "Injecting proxy sidecar")
	for _, workload := range workloads {
		plog.G(ctx).Debugf("Injecting proxy sidecar into workload %q in namespace %q", workload, namespace)
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
			plog.G(ctx).Errorf("Failed to inject proxy sidecar into workload %q in namespace %q: %v", workload, namespace, err)
			return fmt.Errorf("inject sidecar into %s: %w: %w", workload, err, config.ErrEnvoyInject)
		}
		plog.G(ctx).Debugf("Injected proxy sidecar into workload %q in namespace %q", workload, namespace)
		if mapper != nil {
			go mapper.Run()
		}
	}
	plog.StepDone(ctx, "Injected proxy sidecar into %d workloads", len(workloads))
	return nil
}

// CreateOutboundPod ensures the traffic manager pod exists and is ready.
// This is a control-plane responsibility and should be called from the user daemon
// before calling the sudo daemon, so the TunConfigService is available for IP allocation.
func (c *ConnectOptions) CreateOutboundPod(ctx context.Context) error {
	if err := createOutboundPod(ctx, c.clientset, c.ManagerNamespace, c.Image, c.ImagePullSecretName); err != nil {
		// Tag generic deploy failures; an already-classified cause (RBAC, pod-ready
		// timeout, image pull) keeps its own, more specific code.
		return fmt.Errorf("%w: %w", err, config.ErrTrafficManagerDeploy)
	}
	// Grant the manager SA list/watch on pods/services in the workload namespace so its
	// server-side route discovery (WatchNamespaceRoutes) works. Runs every connect
	// (idempotent), covering an already-installed / just-upgraded manager. Best-effort:
	// if the user cannot create this RBAC, discovery degrades to CIDR-only routing.
	ensureRouteRBAC(ctx, c.clientset, c.WorkloadNamespace, c.ManagerNamespace)
	return nil
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
