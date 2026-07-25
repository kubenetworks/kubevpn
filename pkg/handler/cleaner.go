package handler

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func (c *ConnectOptions) setupSignalHandler() {
	stopChan := make(chan os.Signal, 1)
	// SIGKILL and SIGSTOP cannot be caught by signal.Notify, so they are intentionally omitted.
	signal.Notify(stopChan, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	select {
	case <-stopChan:
		c.Cleanup(context.Background())
	case <-c.ctx.Done():
	}
}

// Cleanup releases DHCP leases, leaves proxy resources, and runs rollback functions.
// Uses mutex + flag instead of sync.Once so cleanup can be retried if it fails.
func (c *ConnectOptions) Cleanup(logCtx context.Context) {
	if c == nil {
		return
	}

	c.cleanupMu.Lock()
	if c.cleanedUp {
		c.cleanupMu.Unlock()
		return
	}

	c.configMapStoreMu.Lock()
	store := c.configMapStore
	c.configMapStoreMu.Unlock()
	if store != nil {
		store.Stop()
	}
	const cleanupTimeout = 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	if !c.isDataPlane {
		if err := c.cleanupControlPlane(logCtx, ctx); err != nil {
			c.cleanupMu.Unlock()
			plog.G(logCtx).Warnf("Cleanup incomplete, will retry on next call: %v", err)
			return
		}
	} else {
		c.cleanupDataPlane(logCtx)
	}
	c.cleanedUp = true
	c.cleanupMu.Unlock()
}

// cleanupControlPlane tears down the control-plane side:
// removes ephemeral K8s resources, leaves proxy workloads, and runs rollback functions.
// TUN IP is NOT explicitly released — per DHCP protocol, lease expiry handles reclaim.
// Returns error if critical cleanup steps fail, allowing the caller to retry.
func (c *ConnectOptions) cleanupControlPlane(logCtx context.Context, ctx context.Context) error {
	plog.G(logCtx).Info("Performing cleanup operations")
	if c.clientset != nil {
		_ = c.clientset.CoreV1().Pods(c.ManagerNamespace).Delete(ctx, config.CniNetName, v1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)})
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

// cleanupDataPlane tears down the data-plane side: runs rollback functions,
// stops the networking stack, and cancels the connection context.
func (c *ConnectOptions) cleanupDataPlane(logCtx context.Context) {
	executeRollbackFuncs(logCtx, c.getRollbackFuncs())

	if c.network != nil {
		plog.G(logCtx).Debugf("Stopping network manager")
		c.network.Stop()
	}
	if c.cancel != nil {
		c.cancel()
	}
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
