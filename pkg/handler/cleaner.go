package handler

import (
	"context"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1"
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

func (c *ConnectOptions) Cleanup(ctx context.Context) {
	if c == nil {
		return
	}

	var inUserDaemon bool
	if c.ctx != nil {
		inUserDaemon = true
	}

	c.once.Do(func() {
		if inUserDaemon {
			plog.G(ctx).Info("Performing cleanup operations")
		}
		ctx2, cancel := context.WithTimeout(context.Background(), time.Second*10)
		defer cancel()
		var ips []net.IP
		if c.localTunIPv4 != nil && c.localTunIPv4.IP != nil {
			ips = append(ips, c.localTunIPv4.IP)
		}
		if c.localTunIPv6 != nil && c.localTunIPv6.IP != nil {
			ips = append(ips, c.localTunIPv6.IP)
		}
		if c.dhcp != nil {
			err := c.dhcp.ReleaseIP(ctx2, ips...)
			if err != nil {
				plog.G(ctx).Errorf("Failed to release IP to dhcp, err: %v", err)
			}
		}
		if c.clientset != nil {
			_ = c.clientset.CoreV1().Pods(c.Namespace).Delete(ctx2, config.CniNetName, v1.DeleteOptions{GracePeriodSeconds: pointer.Int64(0)})
		}
		// leave proxy resources
		err := c.LeaveAllProxyResources(ctx2)
		if err != nil {
			plog.G(ctx).Errorf("Leave proxy resources error: %v", err)
		}

		for _, function := range c.getRolloutFunc() {
			if function != nil {
				if err = function(); err != nil {
					plog.G(ctx).Warnf("Rollout function error: %v", err)
				}
			}
		}
		if c.cancel != nil {
			c.cancel()
		}
		if c.dnsConfig != nil {
			if inUserDaemon {
				plog.G(ctx2).Infof("Clearing DNS settings")
			}
			c.dnsConfig.CancelDNS()
		}
	})
}
