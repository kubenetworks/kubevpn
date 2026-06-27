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
		if c.cmInformerStop != nil {
			close(c.cmInformerStop)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
		defer cancel()
		if !c.isDataPlane {
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
					plog.G(logCtx).Errorf("Failed to IPv4 %v IPv6 %v: %v", ipv4, ipv6, err)
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

			for _, function := range c.getRollbackFuncs() {
				if function != nil {
					if err = function(); err != nil {
						plog.G(logCtx).Warnf("Rollback function error: %v", err)
					}
				}
			}
		} else {
			for _, function := range c.getRollbackFuncs() {
				if function != nil {
					if err := function(); err != nil {
						plog.G(logCtx).Warnf("Rollback function error: %v", err)
					}
				}
			}
			if c.dnsConfig != nil {
				plog.G(logCtx).Debugf("Clearing DNS settings")
				c.dnsConfig.CancelDNS()
			}
			if c.cancel != nil {
				c.cancel()
			}
		}
	})
}
