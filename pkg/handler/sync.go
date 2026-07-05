package handler

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// SyncOptions holds configuration for syncthing-based file synchronization.
type SyncOptions struct {
	K8sClient

	WorkloadNamespace string
	// ManagerNamespace is the namespace where the traffic manager runs. It may
	// differ from WorkloadNamespace and is threaded into the cloned workload's
	// nested `kubevpn proxy` so the injected envoy sidecar points its
	// TrafficManagerService at the correct namespace.
	ManagerNamespace string
	Headers          map[string]string
	Workloads        []string
	ExtraRouteInfo   ExtraRouteInfo

	TargetContainer     string
	TargetImage         string
	TargetWorkloadNames map[string]string

	OriginKubeconfigPath string
	LocalDir             string
	RemoteDir            string

	rollbackFuncList []func() error
	ctx              context.Context
	syncthingGUIAddr string
}

// InitClient initializes the Kubernetes client, REST client, and namespace from the given factory.
func (d *SyncOptions) InitClient(f cmdutil.Factory) error {
	var err error
	d.WorkloadNamespace, err = d.K8sClient.InitClient(f)
	return err
}

// SetContext sets the background context used by long-running sync operations.
func (d *SyncOptions) SetContext(ctx context.Context) {
	d.ctx = ctx
}

// DoSync orchestrates syncthing-based file synchronization:
// 1) creates a sync workload (clone of the target with syncthing sidecar)
// 2) waits for it to become ready
// 3) optionally starts local syncthing client for directory sync
func (d *SyncOptions) DoSync(ctx context.Context, kubeconfigJsonBytes []byte, image string) error {
	var args []string
	if len(d.Headers) != 0 {
		args = append(args, "--headers", labels.Set(d.Headers).String())
	}
	plog.StepStart(ctx, "Syncing files")
	for _, workload := range d.Workloads {
		plog.G(ctx).Debugf("Syncing files for workload %q", workload)
		_, controller, err := util.GetTopOwnerObject(ctx, d.factory, d.WorkloadNamespace, workload)
		if err != nil {
			return err
		}
		u := controller.Object.(*unstructured.Unstructured)
		if err = unstructured.SetNestedField(u.UnstructuredContent(), int64(1), "spec", "replicas"); err != nil {
			plog.G(ctx).Warnf("Failed to set replicaset to 1: %v", err)
		}
		clearObjectMetadata(u)
		var newUUID uuid.UUID
		newUUID, err = uuid.NewUUID()
		if err != nil {
			return err
		}
		originName := u.GetName()
		u.SetName(fmt.Sprintf("%s-sync-%s", u.GetName(), newUUID.String()[:5]))
		d.TargetWorkloadNames[workload] = fmt.Sprintf("%s/%s", controller.Mapping.Resource.GroupResource().Resource, u.GetName())
		labelsMap := map[string]string{
			config.ManageBy:   config.ConfigMapPodTrafficManager,
			"owner-ref":       u.GetName(),
			"origin-workload": originName,
		}
		u.SetLabels(labels.Merge(u.GetLabels(), labelsMap))
		u.SetDeletionGracePeriodSeconds(ptr.To[int64](60))
		var path []string
		var spec *v1.PodTemplateSpec
		spec, path, err = util.GetPodTemplateSpecPath(u)
		if err != nil {
			return err
		}

		err = unstructured.SetNestedStringMap(u.Object, labelsMap, "spec", "selector", "matchLabels")
		if err != nil {
			return err
		}
		var client dynamic.Interface
		client, err = d.factory.DynamicClient()
		if err != nil {
			return err
		}
		retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := d.prepareSyncPodSpec(ctx, syncPodSpec{
				spec:       spec,
				u:          u,
				workload:   workload,
				kubeconfig: kubeconfigJsonBytes,
				image:      image,
				args:       args,
				path:       path,
				labels:     labelsMap,
			}); err != nil {
				return err
			}
			_, createErr := client.Resource(controller.Mapping.Resource).Namespace(d.WorkloadNamespace).Create(ctx, u, metav1.CreateOptions{})
			return createErr
		})
		if retryErr != nil {
			return fmt.Errorf("create sync for resource %s failed: %w", workload, retryErr)
		}
		plog.G(ctx).Debugf("Created sync resource %s/%s in target cluster", u.GetObjectKind().GroupVersionKind().GroupKind().String(), u.GetName())
		plog.G(ctx).Debugf("Waiting for sync resource %s/%s to be ready", u.GetObjectKind().GroupVersionKind().GroupKind().String(), u.GetName())
		err = WaitPodReady(ctx, d.clientset.CoreV1().Pods(d.WorkloadNamespace), fields.SelectorFromSet(labelsMap).String(), "")
		if err != nil {
			return err
		}
		// waiting for the freshly-created sync clone to become ready; undo has no
		// meaningful previous revision here and would only disturb the clone.
		_ = util.RolloutStatus(ctx, d.factory, d.WorkloadNamespace, workload, false)

		if d.LocalDir != "" {
			err = d.SyncDir(ctx, fields.SelectorFromSet(labelsMap).String())
			if err != nil {
				return err
			}
		}
	}
	plog.StepDone(ctx, "Synced files for %d workloads", len(d.Workloads))
	return nil
}

// Cleanup deletes sync workloads created by DoSync, waits for the original
// workloads to roll back, and finally executes the rollback functions.
//
// Ordering matters: the rollback functions tear down the session (SSH tunnel and
// temp kubeconfig) that d.factory communicates through, so they must run LAST —
// after the rollout-status wait that still needs that connection. Deletion errors
// are logged but do not prevent the remaining steps from running, ensuring
// resources are cleaned up even when the API server is partially unreachable.
func (d *SyncOptions) Cleanup(ctx context.Context, workloads ...string) error {
	if len(workloads) == 0 {
		for _, v := range d.TargetWorkloadNames {
			workloads = append(workloads, v)
		}
	}
	plog.StepStart(ctx, "Stopping file sync")
	// The daemon registers this Cleanup as a deferred error-path handler BEFORE
	// InitClient sets d.factory (e.g. resolveKubeconfig may fail first). With a
	// nil factory the GetUnstructuredObject / DynamicClient / RolloutStatus calls
	// below would dereference a nil interface and crash the daemon. There is no
	// K8s state to unwind yet, so skip the factory-dependent work — but still run
	// rollbackFuncList to tear down the session (SSH tunnel + temp kubeconfig).
	if d.factory == nil {
		plog.G(ctx).Debug("Skipping workload cleanup: kubernetes client not initialized")
		for _, f := range d.rollbackFuncList {
			if f != nil {
				if err := f(); err != nil {
					plog.G(ctx).Warnf("Failed to exec rollback function: %s", err)
				}
			}
		}
		plog.StepDone(ctx, "Stopped file sync")
		return nil
	}
	syncCount := len(workloads)
	var firstErr error
	for _, workload := range workloads {
		plog.G(ctx).Debugf("Stopping file sync for workload %q", workload)
		object, err := util.GetUnstructuredObject(d.factory, d.WorkloadNamespace, workload)
		if err != nil {
			plog.G(ctx).Errorf("Failed to get unstructured object: %v", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		var client dynamic.Interface
		client, err = d.factory.DynamicClient()
		if err != nil {
			plog.G(ctx).Errorf("Failed to get dynamic client: %v", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		err = client.Resource(object.Mapping.Resource).Namespace(d.WorkloadNamespace).Delete(ctx, object.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			plog.G(ctx).Errorf("Failed to delete sync object: %v", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		plog.G(ctx).Debugf("Deleted sync object %s/%s", object.Mapping.Resource.GroupResource().Resource, object.Name)
	}
	// Wait for the original workloads to roll back BEFORE running the rollback
	// functions. The rollback funcs tear down the session (SSH tunnel + temp
	// kubeconfig) that d.factory talks through; if they run first, RolloutStatus
	// below would hit a closed tunnel and fail with "connection refused".
	for _, workload := range d.Workloads {
		plog.G(ctx).Debugf("Waiting for workload %q to roll back", workload)
		// cleanup rolls the workload back to its original spec; a timed-out
		// rollout must not undo it (that would restore the sync sidecar).
		err := util.RolloutStatus(ctx, d.factory, d.WorkloadNamespace, workload, false)
		if err != nil {
			plog.G(ctx).Warnf("Failed to rollback workload %s: %v", workload, err)
		}
	}
	for _, f := range d.rollbackFuncList {
		if f != nil {
			if err := f(); err != nil {
				plog.G(ctx).Warnf("Failed to exec rollback function: %s", err)
			}
		}
	}
	plog.StepDone(ctx, "Stopped file sync for %d workloads", syncCount)
	return firstErr
}

// AddRollbackFunc registers a function to be called during Cleanup for teardown.
func (d *SyncOptions) AddRollbackFunc(f func() error) {
	d.rollbackFuncList = append(d.rollbackFuncList, f)
}

// GetSyncthingGUIAddr returns the local address of the syncthing GUI for status monitoring.
func (d *SyncOptions) GetSyncthingGUIAddr() string {
	return d.syncthingGUIAddr
}
