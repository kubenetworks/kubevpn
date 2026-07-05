package handler

import (
	"context"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// LeaveAllProxyResources removes all proxy sidecar injections for the current connection's workloads.
func (c *ConnectOptions) LeaveAllProxyResources(ctx context.Context) error {
	if c == nil || c.clientset == nil || c.proxyManager == nil {
		return nil
	}
	return c.proxyManager.LeaveAll(ctx, c.OwnerID)
}

// LeaveResource unpatches the given proxy resources and restores their original pod specs.
func (c *ConnectOptions) LeaveResource(ctx context.Context, resources []Resources, ownerID string) error {
	if c.proxyManager == nil {
		plog.G(ctx).Infof("No proxy manager initialized, skipping leave")
		return nil
	}
	plog.StepStart(ctx, "Removing proxy from workloads")
	if err := c.proxyManager.Leave(ctx, resources, ownerID); err != nil {
		return err
	}
	plog.StepDone(ctx, "Removed proxy from %d workloads", len(resources))
	return nil
}
