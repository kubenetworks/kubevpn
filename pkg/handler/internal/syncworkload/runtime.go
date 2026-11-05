package syncworkload

import (
	"bufio"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/wencaiwulue/kubevpn/v2/pkg/syncthing"
	"k8s.io/client-go/util/retry"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/utils/ptr"

	clusterworkload "github.com/wencaiwulue/kubevpn/v2/pkg/cluster/workload"
	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// Config contains only the values consumed by sync workloads, not session state.
type Config struct {
	WorkloadNamespace string
	ManagerNamespace  string
	Headers           map[string]string
	Workloads         []string
	TargetContainer   string
	TargetImage       string
	LocalDir          string
	RemoteDir         string
}

// Runtime owns cloned workloads and local sync workers. Its context is a child
// of the session context; stopping it never tears down the session's SSH tunnel.
type Runtime struct {
	Config
	clientset           kubernetes.Interface
	factory             cmdutil.Factory
	config              *rest.Config
	targetWorkloadNames map[string]string
	ctx                 context.Context
	cancel              context.CancelFunc
	workers             sync.WaitGroup
	syncthingGUIAddr    string
	clients             []localClient
	startClient         func(context.Context, string, string, string) (localClient, error)
}

// localClient is the shutdown contract required before session teardown.
type localClient interface {
	Stop()
	Done() <-chan struct{}
	Err() error
}

func New(ctx context.Context, cfg Config, clientset kubernetes.Interface, factory cmdutil.Factory, restConfig *rest.Config, targets map[string]string) *Runtime {
	cfg.Headers = maps.Clone(cfg.Headers)
	cfg.Workloads = slices.Clone(cfg.Workloads)
	ctx, cancel := context.WithCancel(ctx)
	ownedTargets := make(map[string]string, len(targets))
	maps.Copy(ownedTargets, targets)
	return &Runtime{Config: cfg, clientset: clientset, factory: factory, config: restConfig,
		targetWorkloadNames: ownedTargets, ctx: ctx, cancel: cancel,
		startClient: func(ctx context.Context, dir, local, remote string) (localClient, error) {
			return syncthing.StartClient(ctx, dir, local, remote)
		}}
}

// TargetWorkloadNames returns a snapshot for status and persistence consumers.
func (d *Runtime) TargetWorkloadNames() map[string]string {
	return maps.Clone(d.targetWorkloadNames)
}

// Stop cancels and joins both IP trackers and local Syncthing applications,
// including their early services, before the caller tears down the session.
func (d *Runtime) Stop() {
	d.cancel()
	d.workers.Wait()
	for _, client := range d.clients {
		client.Stop()
		<-client.Done()
	}
}

// DoSync orchestrates syncthing-based file synchronization:
// 1) creates a sync workload (clone of the target with syncthing sidecar)
// 2) waits for it to become ready
// 3) optionally starts local syncthing client for directory sync
func (d *Runtime) DoSync(ctx context.Context, kubeconfigJsonBytes []byte, image string) error {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(d.ctx, cancel)
	defer func() { stop(); cancel() }()
	if err := d.ctx.Err(); err != nil {
		return err
	}
	var args []string
	if len(d.Headers) != 0 {
		args = append(args, "--headers", labels.Set(d.Headers).String())
	}
	plog.StepStart(ctx, "Syncing files")
	for _, workload := range d.Workloads {
		plog.G(ctx).Debugf("Syncing files for workload %q", workload)
		_, controller, err := clusterworkload.GetTopOwnerObject(ctx, d.factory, d.WorkloadNamespace, workload)
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
		d.targetWorkloadNames[workload] = fmt.Sprintf("%s/%s", controller.Mapping.Resource.GroupResource().Resource, u.GetName())
		labelsMap := map[string]string{
			config.ManageBy:   config.ConfigMapPodTrafficManager,
			"owner-ref":       u.GetName(),
			"origin-workload": originName,
		}
		u.SetLabels(labels.Merge(u.GetLabels(), labelsMap))
		u.SetDeletionGracePeriodSeconds(ptr.To[int64](60))
		var path []string
		var spec *v1.PodTemplateSpec
		spec, path, err = clusterworkload.GetPodTemplateSpecPath(u)
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
		err = clusterworkload.WaitPodReady(ctx, d.clientset.CoreV1().Pods(d.WorkloadNamespace), fields.SelectorFromSet(labelsMap).String(), "")
		if err != nil {
			return err
		}
		// waiting for the freshly-created sync clone to become ready; undo has no
		// meaningful previous revision here and would only disturb the clone.
		_ = clusterworkload.RolloutStatus(ctx, d.factory, d.WorkloadNamespace, workload, false)

		if err = d.waitSidecarConnected(ctx, labelsMap); err != nil {
			plog.G(ctx).Debugf("wait for VPN sidecar connected: %v", err)
		}

		if d.LocalDir != "" {
			err = d.syncDir(ctx, fields.SelectorFromSet(labelsMap).String())
			if err != nil {
				return err
			}
		}
	}
	plog.StepDone(ctx, "Synced files for %d workloads", len(d.Workloads))
	return nil
}

// waitSidecarConnected streams the VPN sidecar container's logs until it prints
// the connection slogan, meaning server-side inject is complete and the envoy
// config has been written to the ConfigMap. Without this, DoSync returns as soon
// as the pod is Running — before the sidecar finishes connect + inject — and a
// status query immediately after would see ProxyList:null.
func (d *Runtime) waitSidecarConnected(ctx context.Context, labelsMap map[string]string) error {
	const timeout = 2 * time.Minute
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	list, err := util.GetRunningPodList(ctx, d.clientset, d.WorkloadNamespace,
		fields.SelectorFromSet(labelsMap).String())
	if err != nil || len(list) == 0 {
		return fmt.Errorf("no running sync pod: %w", err)
	}
	stream, err := d.clientset.CoreV1().Pods(d.WorkloadNamespace).GetLogs(list[0].Name, &v1.PodLogOptions{
		Container: config.ContainerSidecarVPN,
		Follow:    true,
	}).Stream(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()

	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), config.Slogan) {
			return nil
		}
	}
	if err = scanner.Err(); err != nil {
		return err
	}
	return fmt.Errorf("VPN sidecar log ended without %q", config.Slogan)
}

// Cleanup deletes sync clones, waits for the original workloads, and stops local
// workers. The caller must keep its session/SSH connection alive until it returns.
func (d *Runtime) Cleanup(ctx context.Context, workloads ...string) error {
	defer d.Stop()
	if len(workloads) == 0 {
		for _, v := range d.targetWorkloadNames {
			workloads = append(workloads, v)
		}
	}
	if d.factory == nil {
		plog.G(ctx).Debug("Skipping workload cleanup: kubernetes client not initialized")
		// Session teardown belongs to the caller.
		return nil
	}
	var firstErr error
	for _, workload := range workloads {
		plog.G(ctx).Debugf("Stopping file sync for workload %q", workload)
		object, err := clusterworkload.GetUnstructuredObject(d.factory, d.WorkloadNamespace, workload)
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
		err := clusterworkload.RolloutStatus(ctx, d.factory, d.WorkloadNamespace, workload, false)
		if err != nil {
			plog.G(ctx).Warnf("Failed to rollback workload %s: %v", workload, err)
		}
	}
	// Session teardown belongs to the caller.
	return firstErr
}

// GetSyncthingGUIAddr returns the local address of the syncthing GUI for status monitoring.
func (d *Runtime) GetSyncthingGUIAddr() string {
	return d.syncthingGUIAddr
}
