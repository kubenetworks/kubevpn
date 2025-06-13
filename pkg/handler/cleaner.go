package handler

import (
	"context"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func (c *ConnectOptions) setupSignalHandler() {
	var stopChan = make(chan os.Signal)
	signal.Notify(stopChan, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL)
	select {
	case <-stopChan:
		c.Cleanup(context.Background())
	case <-c.ctx.Done():
	}
}

func (c *ConnectOptions) Cleanup(logCtx context.Context) {
	if c == nil {
		return
	}

	// only root daemon really do connect
	// root daemon: data plane
	// user daemon: control plane
	var userDaemon = true
	if c.ctx != nil {
		userDaemon = false
	}

	c.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
		defer cancel()
		if userDaemon {
			plog.G(logCtx).Info("Performing cleanup operations")
			var ipv4, ipv6 net.IP
			if c.localTunIPv4 != nil && c.localTunIPv4.IP != nil {
				ipv4 = c.localTunIPv4.IP
			}
			if c.localTunIPv6 != nil && c.localTunIPv6.IP != nil {
				ipv6 = c.localTunIPv6.IP
			}
			if c.dhcp != nil {
				err := c.dhcp.ReleaseIP(ctx, ipv4, ipv6)
				if err != nil {
					plog.G(logCtx).Errorf("Failed to release IP to DHCP server: %v", err)
				} else {
					plog.G(logCtx).Infof("Releaseed IPv4 %v IPv6 %v to DHCP server", ipv4, ipv6)
				}
			}
			if c.clientset != nil {
				_ = c.clientset.CoreV1().Pods(c.Namespace).Delete(ctx, config.CniNetName, v1.DeleteOptions{GracePeriodSeconds: pointer.Int64(0)})
				_ = c.clientset.BatchV1().Jobs(c.Namespace).Delete(ctx, config.ConfigMapPodTrafficManager, v1.DeleteOptions{GracePeriodSeconds: pointer.Int64(0)})
			}
			// leave proxy resources
			err := c.LeaveAllProxyResources(ctx)
			if err != nil {
				plog.G(logCtx).Errorf("Leave proxy resources error: %v", err)
			}

			for _, function := range c.getRolloutFunc() {
				if function != nil {
					if err = function(); err != nil {
						plog.G(logCtx).Warnf("Rollout function error: %v", err)
					}
				}
			}
		} else {
			if c.cancel != nil {
				c.cancel()
			}

			for _, function := range c.getRolloutFunc() {
				if function != nil {
					if err := function(); err != nil {
						plog.G(logCtx).Warnf("Rollout function error: %v", err)
					}
				}
			}
			if c.dnsConfig != nil {
				plog.G(logCtx).Infof("Clearing DNS settings")
				c.dnsConfig.CancelDNS()
			}
		}
	})
}
