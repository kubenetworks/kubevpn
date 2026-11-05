package handler

import (
	"context"

	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/handler/internal/syncworkload"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// SyncOptions holds CLI configuration and adapts the daemon session to the sync runtime.
type SyncOptions struct {
	K8sClient
	WorkloadNamespace    string
	ManagerNamespace     string
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
	ctx     context.Context
	runtime *syncworkload.Runtime
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

func (d *SyncOptions) syncRuntime(ctx context.Context) *syncworkload.Runtime {
	if d.runtime == nil {
		if d.ctx != nil {
			ctx = d.ctx
		}
		d.runtime = syncworkload.New(ctx, syncworkload.Config{
			WorkloadNamespace: d.WorkloadNamespace,
			ManagerNamespace:  d.ManagerNamespace,
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

func (d *SyncOptions) DoSync(ctx context.Context, kubeconfigJSON []byte, image string) error {
	r := d.syncRuntime(ctx)
	// Preserve partial clone state on failure for status and persisted cleanup.
	defer func() { d.TargetWorkloadNames = r.TargetWorkloadNames() }()
	return r.DoSync(ctx, kubeconfigJSON, image)
}

// Cleanup keeps the session/SSH connection alive until workload cleanup and
// local worker cancellation have completed, including the pre-init error path.
func (d *SyncOptions) Cleanup(ctx context.Context, workloads ...string) error {
	plog.StepStart(ctx, "Stopping file sync")
	uninitialized := d.runtime == nil && d.factory == nil
	count := len(workloads)
	var err error
	if uninitialized {
		plog.G(ctx).Debug("Skipping workload cleanup: kubernetes client not initialized")
	} else {
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

func (d *SyncOptions) GetSyncthingGUIAddr() string {
	if d.runtime == nil {
		return ""
	}
	return d.runtime.GetSyncthingGUIAddr()
}

func (d *SyncOptions) ConvertApiServerToNodeIP(ctx context.Context, kubeconfigBytes []byte) ([]byte, error) {
	// Conversion precedes DoSync; do not freeze runtime configuration here.
	return syncworkload.ConvertAPIServerToNodeIP(ctx, d.clientset, d.config, d.WorkloadNamespace, kubeconfigBytes)
}
