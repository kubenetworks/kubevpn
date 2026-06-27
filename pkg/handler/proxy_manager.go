package handler

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// ProxyManager manages the lifecycle of proxy workloads (sidecar injection,
// leave/unpatch operations) for a VPN connection.
type ProxyManager struct {
	factory   cmdutil.Factory
	clientset kubernetes.Interface
	managerNamespace string

	mu        sync.Mutex
	workloads ProxyList
}

// NewProxyManager creates a ProxyManager with the given Kubernetes factory,
// clientset, and manager namespace.
func NewProxyManager(factory cmdutil.Factory, clientset kubernetes.Interface, managerNamespace string) *ProxyManager {
	return &ProxyManager{
		factory:   factory,
		clientset: clientset,
		managerNamespace: managerNamespace,
		workloads: make(ProxyList, 0),
	}
}

// Add appends a proxy workload to the managed list.
func (pm *ProxyManager) Add(proxy *Proxy) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.workloads.Add(proxy)
}

// Remove removes a proxy workload by namespace and workload name.
func (pm *ProxyManager) Remove(ns, workload string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.workloads.Remove(ns, workload)
}

// Resources returns a copy of the current proxy workload list.
func (pm *ProxyManager) Resources() ProxyList {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	result := make(ProxyList, len(pm.workloads))
	copy(result, pm.workloads)
	return result
}

// IsMe reports whether the proxy manager owns a proxy matching the given namespace, UID, and headers.
func (pm *ProxyManager) IsMe(ns, uid string, headers map[string]string) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.workloads.IsMe(ns, uid, headers)
}

// LeaveAll removes all proxy sidecar injections.
func (pm *ProxyManager) LeaveAll(ctx context.Context, localIPv4 string) error {
	resources := pm.Resources().ToResources()
	if len(resources) == 0 {
		plog.G(ctx).Infof("No proxy resources found")
		return nil
	}
	return pm.Leave(ctx, resources, localIPv4)
}

// Leave unpatches the given proxy resources and restores their original pod specs.
func (pm *ProxyManager) Leave(ctx context.Context, resources []Resources, v4 string) error {
	plog.G(ctx).Infof("Leaving %d proxy resources with local IP %s", len(resources), v4)
	var errs []error
	for _, workload := range resources {
		// deployments.apps.ry-server --> deployments.apps/ry-server
		object, controller, err := util.GetTopOwnerObject(ctx, pm.factory, workload.Namespace, workload.Workload)
		if err != nil {
			plog.G(ctx).Errorf("Failed to get unstructured object: %v", err)
			errs = append(errs, err)
			continue
		}
		nodeID := fmt.Sprintf("%s.%s", object.Mapping.Resource.GroupResource().String(), object.Name)
		var empty bool
		empty, err = inject.UnpatchContainer(ctx, nodeID, pm.factory, pm.clientset.CoreV1().ConfigMaps(pm.managerNamespace), controller, func(isFargateMode bool, rule *controlplane.Rule) bool {
			if isFargateMode {
				return pm.IsMe(workload.Namespace, util.ConvertWorkloadToUID(workload.Workload), rule.Headers)
			}
			return rule.LocalTunIPv4 == v4
		})
		if err != nil {
			plog.G(ctx).Errorf("Failed to leave workload %s in namespace %s: %v", workload.Workload, workload.Namespace, err)
			errs = append(errs, err)
			continue
		}
		if empty && util.IsK8sService(object) {
			if err = inject.ModifyServiceTargetPort(ctx, pm.clientset, workload.Namespace, object.Name, map[int32]int32{}); err != nil {
				errs = append(errs, err)
			}
		}
		pm.Remove(workload.Namespace, workload.Workload)
		plog.G(ctx).Infof("Left workload %s in namespace %s", workload.Workload, workload.Namespace)
	}
	return errors.NewAggregate(errs)
}
