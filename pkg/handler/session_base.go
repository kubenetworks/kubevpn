package handler

import (
	"context"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// SessionBase holds the fields that are shared between ControlSession (ConnectOptions, user daemon)
// and DataSession (root daemon). Both types embed this struct.
//
// Only non-literal-set fields live here: the K8sClient bundle, rollback lifecycle, and the lazy
// ConfigMapStore. Identity fields (ManagerNamespace, WorkloadNamespace, OwnerID, ConnectionID,
// ExtraRouteInfo, Image, etc.) are kept flat on each concrete type so that ~89 composite literals
// across test files do not need to be updated when constructing either type.
type SessionBase struct {
	K8sClient

	// Shared lifecycle. rollbackList provides the thread-safe AddRollbackFunc /
	// getRollbackFuncs registry (also embedded by SyncOptions).
	rollbackList
	cleanupMu sync.Mutex
	cleanedUp bool

	// configMapStore is lazily initialized on first access via getConfigMapStore().
	// It is separate from the identity fields because it depends on ManagerNamespace,
	// which may be updated by detectAndSetManagerNamespace after InitClient returns
	// (user daemon path).
	configMapStore   *ConfigMapStore
	configMapStoreMu sync.Mutex
}

// getConfigMapStore returns the ConfigMapStore for the given namespace, creating it lazily on
// first access. managerNamespace is passed explicitly because ManagerNamespace is a flat field
// on the concrete type, not in SessionBase.
func (sb *SessionBase) getConfigMapStore(managerNamespace string) *ConfigMapStore {
	sb.configMapStoreMu.Lock()
	defer sb.configMapStoreMu.Unlock()
	if sb.configMapStore == nil {
		sb.configMapStore = newConfigMapStore(sb.clientset, managerNamespace)
	}
	return sb.configMapStore
}

// GetRunningPodList returns the running traffic manager pods in the given namespace.
func (sb *SessionBase) GetRunningPodList(ctx context.Context, managerNamespace string) ([]v1.Pod, error) {
	label := "app=" + config.ConfigMapPodTrafficManager
	return util.GetRunningPodList(ctx, sb.clientset, managerNamespace, label)
}

// cleanup holds the common cleanup mechanics: mutex gate, configMapStore stop, and timeout.
// The passed cleanupFn is the role-specific logic (control-plane or data-plane).
// Uses mutex + flag instead of sync.Once so cleanup can be retried if the cleanupFn fails.
func (sb *SessionBase) cleanup(logCtx context.Context, cleanupFn func(ctx context.Context) error) {
	sb.cleanupMu.Lock()
	if sb.cleanedUp {
		sb.cleanupMu.Unlock()
		return
	}

	sb.configMapStoreMu.Lock()
	store := sb.configMapStore
	sb.configMapStoreMu.Unlock()
	if store != nil {
		store.Stop()
	}

	const cleanupTimeout = 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	if err := cleanupFn(ctx); err != nil {
		sb.cleanupMu.Unlock()
		plog.G(logCtx).Warnf("Cleanup incomplete, will retry on next call: %v", err)
		return
	}
	sb.cleanedUp = true
	sb.cleanupMu.Unlock()
}
