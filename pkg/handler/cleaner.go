package handler

import (
	"context"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// Cleanup releases DHCP leases, leaves proxy resources, and runs rollback functions.
// ConnectOptions is always the control-plane (user daemon) session type; Cleanup always
// runs cleanupControlPlane. Uses the shared SessionBase.cleanup for mutex gating.
func (c *ConnectOptions) Cleanup(logCtx context.Context) {
	if c == nil {
		return
	}
	c.SessionBase.cleanup(logCtx, func(ctx context.Context) error {
		return c.cleanupControlPlane(logCtx, ctx)
	})
}

// cleanupControlPlane tears down the control-plane side:
// removes ephemeral K8s resources, leaves proxy workloads, and runs rollback functions.
// TUN IP is NOT explicitly released — per DHCP protocol, lease expiry handles reclaim.
// Returns error if critical cleanup steps fail, allowing the caller to retry.
func (c *ConnectOptions) cleanupControlPlane(logCtx context.Context, ctx context.Context) error {
	// Debug, not Info: in quit/disconnect this marker duplicates the enclosing
	// "Cleaning up connections"/"Disconnecting" step, so keep it out of the CLI
	// stream while preserving it in the daemon log file for diagnostics.
	plog.G(logCtx).Debug("Performing cleanup operations")
	if c.clientset != nil {
		_ = c.clientset.BatchV1().Jobs(c.ManagerNamespace).Delete(ctx, config.ConfigMapPodTrafficManager, v1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)})
	}
	// leave proxy resources
	if err := c.LeaveAllProxyResources(ctx); err != nil {
		plog.G(logCtx).Errorf("Leave proxy resources error: %v", err)
		return err
	}

	executeRollbackFuncs(logCtx, c.getRollbackFuncs())
	return nil
}

// executeRollbackFuncs runs each non-nil rollback function, logging warnings on errors.
func executeRollbackFuncs(logCtx context.Context, funcs []func() error) {
	for _, fn := range funcs {
		if fn != nil {
			if err := fn(); err != nil {
				plog.G(logCtx).Warnf("Rollback function error: %v", err)
			}
		}
	}
}
