package handler

import (
	"context"
	"errors"
	"maps"
	"net/netip"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	informerv1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func NewMapper(clientset kubernetes.Interface, ns string, labels string, headers map[string]string, workload string, cmInformer cache.SharedInformer) *Mapper {
	ctx, cancelFunc := context.WithCancel(context.Background())
	return &Mapper{
		ns:         ns,
		headers:    headers,
		workload:   workload,
		labels:     labels,
		ctx:        ctx,
		cancel:     cancelFunc,
		clientset:  clientset,
		cmInformer: cmInformer,
	}
}

// Mapper manages SSH reverse tunnels for Fargate/Service mode port forwarding.
type Mapper struct {
	ns       string
	headers  map[string]string
	workload string
	labels   string

	ctx        context.Context
	cancel     context.CancelFunc
	clientset  kubernetes.Interface
	cmInformer cache.SharedInformer
}

// Run uses informer watches on ConfigMap and Pods to react to changes
// instead of polling. A reconcile ticker acts as a fallback safety net.
func (m *Mapper) Run() {
	if m == nil {
		return
	}
	podTunnels := &sync.Map{}
	defer func() {
		podTunnels.Range(func(_, value any) bool {
			value.(context.CancelFunc)()
			return true
		})
	}()

	// reconcile channel — informer events and ticker both feed into this
	reconcileCh := make(chan struct{}, 1)
	triggerReconcile := func() {
		select {
		case reconcileCh <- struct{}{}:
		default:
		}
	}

	// Shared ConfigMap informer — reuse from ConnectOptions, no extra watch connection
	m.cmInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ any) { triggerReconcile() },
		UpdateFunc: func(_, _ any) { triggerReconcile() },
		DeleteFunc: func(_ any) { triggerReconcile() },
	})

	// Watch Pods matching the service selector
	podInformer := informerv1.NewFilteredPodInformer(
		m.clientset, m.ns, 0, cache.Indexers{},
		func(options *metav1.ListOptions) {
			options.LabelSelector = m.labels
		},
	)
	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ any) { triggerReconcile() },
		UpdateFunc: func(_, _ any) { triggerReconcile() },
		DeleteFunc: func(_ any) { triggerReconcile() },
	})
	go podInformer.Run(m.ctx.Done())

	// Fallback ticker — ensures reconciliation even if an informer event is missed
	ticker := time.NewTicker(time.Second * 30)
	defer ticker.Stop()

	// Initial reconcile
	triggerReconcile()

	var lastPortMapping map[int32]int32
	for m.ctx.Err() == nil {
		select {
		case <-reconcileCh:
		case <-ticker.C:
		case <-m.ctx.Done():
			return
		}

		portMapping, err := m.getPortMappingFromCache()
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				plog.G(m.ctx).Errorf("Failed to get port mapping: %v", err)
			}
			continue
		}

		if !maps.Equal(portMapping, lastPortMapping) {
			cancelAllTunnels(podTunnels)
		}
		lastPortMapping = portMapping

		m.reconcilePodsFromInformer(podTunnels, podInformer, portMapping)
	}
}

// reconcilePodsFromInformer uses the pod informer's cache instead of listing from API server.
func (m *Mapper) reconcilePodsFromInformer(podTunnels *sync.Map, podInformer cache.SharedInformer, portMapping map[int32]int32) {
	activePods := sets.New[string]()
	for _, obj := range podInformer.GetStore().List() {
		pod, ok := obj.(*v1.Pod)
		if !ok || pod.Status.Phase != v1.PodRunning || pod.DeletionTimestamp != nil {
			continue
		}

		activePods.Insert(pod.Name)
		if _, exists := podTunnels.Load(pod.Name); exists {
			continue
		}

		containerNames := sets.New[string]()
		for _, container := range pod.Spec.Containers {
			containerNames.Insert(container.Name)
		}
		if !containerNames.HasAny(config.ContainerSidecarVPN, config.ContainerSidecarEnvoyProxy) {
			plog.G(m.ctx).Infof("Pod %s no longer has sidecar containers", pod.Name)
			continue
		}

		podIP, err := netip.ParseAddr(pod.Status.PodIP)
		if err != nil {
			continue
		}

		ctx, cancel := context.WithCancel(m.ctx)
		podTunnels.Store(pod.Name, cancel)
		sshServer := netip.AddrPortFrom(podIP, 2222)
		go m.startTunnels(ctx, sshServer, portMapping)
	}

	podTunnels.Range(func(key, value any) bool {
		if !activePods.Has(key.(string)) {
			value.(context.CancelFunc)()
			podTunnels.Delete(key)
		}
		return true
	})
}

// getPortMappingFromCache reads the ConfigMap from the shared informer cache.
func (m *Mapper) getPortMappingFromCache() (map[int32]int32, error) {
	items := m.cmInformer.GetStore().List()
	if len(items) == 0 {
		return nil, nil
	}
	configMap, ok := items[0].(*v1.ConfigMap)
	if !ok {
		return nil, nil
	}

	return m.extractPortMapping(configMap)
}

func (m *Mapper) extractPortMapping(configMap *v1.ConfigMap) (map[int32]int32, error) {
	var virtuals []*controlplane.Virtual
	if str, ok := configMap.Data[config.KeyEnvoy]; ok {
		if err := yaml.Unmarshal([]byte(str), &virtuals); err != nil {
			return nil, err
		}
	}

	result := make(map[int32]int32)
	for _, virtual := range virtuals {
		if util.ConvertWorkloadToUID(m.workload) != virtual.UID || m.ns != virtual.Namespace {
			continue
		}
		for _, rule := range virtual.Rules {
			if !maps.Equal(m.headers, rule.Headers) {
				continue
			}
			for _, pm := range rule.ParsePortMap() {
				result[pm.LocalPort] = pm.EnvoyPort
			}
		}
	}
	return result, nil
}

// startTunnels creates SSH port-forward tunnels for each port mapping entry.
func (m *Mapper) startTunnels(ctx context.Context, sshServer netip.AddrPort, portMapping map[int32]int32) {
	for containerPort, envoyPort := range portMapping {
		go func(containerPort, envoyPort int32) {
			local := netip.AddrPortFrom(netip.IPv4Unspecified(), uint16(containerPort))
			remote := netip.AddrPortFrom(netip.IPv4Unspecified(), uint16(envoyPort))
			for ctx.Err() == nil {
				ctx2, cancel := context.WithCancel(ctx)
				_ = ssh.ExposeLocalPortToRemote(ctx2, sshServer, remote, local)
				cancel()
				time.Sleep(time.Second * 2)
			}
		}(containerPort, envoyPort)
	}
}


func cancelAllTunnels(tunnels *sync.Map) {
	tunnels.Range(func(_, value any) bool {
		value.(context.CancelFunc)()
		return true
	})
	tunnels.Clear()
}

func (m *Mapper) Stop() {
	if m == nil || m.cancel == nil {
		return
	}
	m.cancel()
}
