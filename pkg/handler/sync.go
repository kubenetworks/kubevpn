package handler

import (
	"context"
	"errors"

	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/handler/internal/syncworkload"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

type syncRouteController interface {
	GetOwnerID() string
	LeaveResource(context.Context, []Resources, string) error
}

// SyncOptions holds CLI configuration and adapts the daemon session to the sync runtime.
type SyncOptions struct {
	K8sClient
	WorkloadNamespace    string
	Headers              map[string]string
	Workloads            []string
	ExtraRouteInfo       ExtraRouteInfo
	TargetContainer      string
	TargetImage          string
	TargetWorkloadNames  map[string]string
	OriginKubeconfigPath string
	LocalDir             string
	RemoteDir            string

	rollbackList
	ctx             context.Context
	runtime         *syncworkload.Runtime
	routeController syncRouteController
}

func (d *SyncOptions) InitClient(f cmdutil.Factory) error {
	var err error
	d.WorkloadNamespace, err = d.K8sClient.InitClient(f)
	if err != nil {
		// InitClient stores the factory before validating its config. A partial
		// initialization has no workloads to clean and must only unwind the session.
		d.K8sClient = K8sClient{}
	}
	return err
}

func (d *SyncOptions) SetContext(ctx context.Context) {
	d.ctx = ctx
}

func (d *SyncOptions) SetRouteController(controller syncRouteController) {
	d.routeController = controller
}

func (d *SyncOptions) syncRuntime(ctx context.Context) *syncworkload.Runtime {
	if d.runtime == nil {
		if d.ctx != nil {
			ctx = d.ctx
		}
		d.runtime = syncworkload.New(ctx, syncworkload.Config{
			WorkloadNamespace: d.WorkloadNamespace,
			Headers:           d.Headers,
			Workloads:         d.Workloads,
			TargetContainer:   d.TargetContainer,
			TargetImage:       d.TargetImage,
			LocalDir:          d.LocalDir,
			RemoteDir:         d.RemoteDir,
		}, d.clientset, d.factory, d.config, d.TargetWorkloadNames)
	}
	return d.runtime
}

func (d *SyncOptions) DoSync(ctx context.Context, image string) error {
	r := d.syncRuntime(ctx)
	// Preserve partial clone state on failure for status and persisted cleanup.
	defer func() { d.TargetWorkloadNames = r.TargetWorkloadNames() }()
	return r.DoSync(ctx, image)
}

// Cleanup removes routes before deleting clones, then stops local workers and
// tears down the session. A failed route cleanup preserves the clone for retry.
func (d *SyncOptions) Cleanup(ctx context.Context, workloads ...string) error {
	plog.StepStart(ctx, "Stopping file sync")
	uninitialized := d.runtime == nil && d.factory == nil
	count := len(workloads)
	routeErr := d.cleanupRoutes(ctx, workloads)
	var err error
	switch {
	case routeErr != nil:
		err = routeErr
		if d.runtime != nil {
			d.runtime.Stop()
		}
	case uninitialized:
		plog.G(ctx).Debug("Skipping workload cleanup: kubernetes client not initialized")
	default:
		r := d.syncRuntime(ctx)
		if count == 0 {
			count = len(r.TargetWorkloadNames())
		}
		err = r.Cleanup(ctx, workloads...)
	}
	executeRollbackFuncs(ctx, d.getRollbackFuncs())
	if uninitialized {
		plog.StepDone(ctx, "Stopped file sync")
	} else {
		plog.StepDone(ctx, "Stopped file sync for %d workloads", count)
	}
	return err
}

func (d *SyncOptions) cleanupRoutes(ctx context.Context, requested []string) error {
	if d.routeController == nil {
		return nil
	}
	sources := make([]string, 0, len(d.Workloads))
	if len(requested) == 0 {
		sources = append(sources, d.Workloads...)
	} else {
		reverse := make(map[string]string, len(d.TargetWorkloadNames))
		for source, clone := range d.TargetWorkloadNames {
			reverse[clone] = source
		}
		for _, workload := range requested {
			if _, ok := d.TargetWorkloadNames[workload]; ok {
				sources = append(sources, workload)
			} else if source := reverse[workload]; source != "" {
				sources = append(sources, source)
			}
		}
	}
	seen := make(map[string]struct{}, len(sources))
	var errs []error
	for _, source := range sources {
		if _, ok := seen[source]; ok {
			continue
		}
		seen[source] = struct{}{}
		resource := Resources{Namespace: d.WorkloadNamespace, Workload: source}
		if err := d.routeController.LeaveResource(ctx, []Resources{resource}, d.routeController.GetOwnerID()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (d *SyncOptions) GetSyncthingGUIAddr() string {
	if d.runtime == nil {
		return ""
	}
	return d.runtime.GetSyncthingGUIAddr()
}
