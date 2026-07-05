package handler

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// ProxyManager manages the lifecycle of proxy workloads (sidecar injection,
// leave/unpatch operations) for a VPN connection.
type ProxyManager struct {
	factory          cmdutil.Factory
	clientset        kubernetes.Interface
	managerNamespace string

	mu        sync.Mutex
	workloads ProxyList
}

// newProxyManager creates a ProxyManager with the given Kubernetes factory,
// clientset, and manager namespace.
func newProxyManager(factory cmdutil.Factory, clientset kubernetes.Interface, managerNamespace string) *ProxyManager {
	return &ProxyManager{
		factory:          factory,
		clientset:        clientset,
		managerNamespace: managerNamespace,
		workloads:        make(ProxyList, 0),
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

// LeaveAll removes all proxy sidecar injections.
func (pm *ProxyManager) LeaveAll(ctx context.Context, ownerID string) error {
	resources := pm.Resources().ToResources()
	if len(resources) == 0 {
		plog.G(ctx).Infof("No proxy resources found")
		return nil
	}
	return pm.Leave(ctx, resources, ownerID)
}

// Leave unpatches the given proxy resources and restores their original pod specs.
func (pm *ProxyManager) Leave(ctx context.Context, resources []Resources, ownerID string) error {
	plog.G(ctx).Infof("Leaving %d proxy resources with OwnerID %s", len(resources), ownerID)
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
		empty, err = inject.UnpatchContainer(ctx, nodeID, pm.factory, pm.clientset.CoreV1().ConfigMaps(pm.managerNamespace), controller, ownerID)
		if err != nil {
			plog.G(ctx).Errorf("Failed to leave workload %s in namespace %s: %v", workload.Workload, workload.Namespace, err)
			errs = append(errs, err)
			continue
		}
		if empty && util.IsK8sService(object) {
			if err = inject.RestoreServiceTargetPort(ctx, pm.clientset, workload.Namespace, object.Name); err != nil {
				errs = append(errs, err)
			}
		}
		pm.Remove(workload.Namespace, workload.Workload)
		plog.G(ctx).Infof("Left workload %s in namespace %s", workload.Workload, workload.Namespace)
	}
	agg := errors.NewAggregate(errs)
	if agg == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", agg, config.ErrCleanupFailed)
}
