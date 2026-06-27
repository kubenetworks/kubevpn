package handler

import (
	"context"
	"net"
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
	signal.Notify(stopChan, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL)
	select {
	case <-stopChan:
		c.Cleanup(context.Background())
	case <-c.ctx.Done():
	}
}

// Cleanup releases DHCP leases, leaves proxy resources, and runs rollback functions.
func (c *ConnectOptions) Cleanup(logCtx context.Context) {
	if c == nil {
		return
	}

	c.once.Do(func() {
		if c.configMapStore != nil {
			c.configMapStore.Stop()
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
		defer cancel()

		if !c.isDataPlane {
			c.cleanupControlPlane(logCtx, ctx)
		} else {
			c.cleanupDataPlane(logCtx)
		}
	})
}

// cleanupControlPlane tears down the control-plane side: releases DHCP leases,
// removes ephemeral K8s resources, leaves proxy workloads, and runs rollback functions.
func (c *ConnectOptions) cleanupControlPlane(logCtx context.Context, ctx context.Context) {
	plog.G(logCtx).Info("Performing cleanup operations")
	var ipv4, ipv6 net.IP
	if c.LocalTunIPv4 != nil && c.LocalTunIPv4.IP != nil {
		ipv4 = c.LocalTunIPv4.IP
	}
	if c.LocalTunIPv6 != nil && c.LocalTunIPv6.IP != nil {
		ipv6 = c.LocalTunIPv6.IP
	}
	if c.dhcp != nil {
		err := c.dhcp.ReleaseIP(ctx, ipv4, ipv6)
		if err != nil {
			plog.G(logCtx).Errorf("Failed to release IPv4 %v IPv6 %v: %v", ipv4, ipv6, err)
		} else {
			plog.G(logCtx).Infof("Released IPv4 %v IPv6 %v", ipv4, ipv6)
		}
	}
	if c.clientset != nil {
		_ = c.clientset.CoreV1().Pods(c.ManagerNamespace).Delete(ctx, config.CniNetName, v1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)})
		_ = c.clientset.BatchV1().Jobs(c.ManagerNamespace).Delete(ctx, config.ConfigMapPodTrafficManager, v1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)})
	}
	// leave proxy resources
	err := c.LeaveAllProxyResources(ctx)
	if err != nil {
		plog.G(logCtx).Errorf("Leave proxy resources error: %v", err)
	}

	executeRollbackFuncs(logCtx, c.getRollbackFuncs())
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
