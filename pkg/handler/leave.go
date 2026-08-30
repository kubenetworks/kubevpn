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
// It prefers server-side unpatching (the manager removes the envoy rule and sidecar with
// its own ServiceAccount, over the VPN), falling back to the local proxyManager path when
// the manager is unreachable / too old / lacks RBAC. Server-side injection leaves the
// client without a proxyManager record for mesh workloads, so leave can no longer rely on
// that record — the workloads come from the request instead.
func (c *ConnectOptions) LeaveResource(ctx context.Context, resources []Resources, ownerID string) error {
	if len(resources) == 0 {
		return nil
	}
	plog.StepStart(ctx, "Removing proxy from workloads")

	// Group by namespace: one LeaveInject call per namespace.
	byNamespace := make(map[string][]string)
	var order []string
	for _, r := range resources {
		if _, ok := byNamespace[r.Namespace]; !ok {
			order = append(order, r.Namespace)
		}
		byNamespace[r.Namespace] = append(byNamespace[r.Namespace], r.Workload)
	}

	for _, ns := range order {
		workloads := byNamespace[ns]
		handled, err := c.leaveViaManager(ctx, ns, workloads, ownerID)
		if err != nil {
			return err
		}
		if handled {
			// Manager did the cluster writes; stop any local Mapper for these workloads.
			c.stopServiceMappers(ns, workloads)
			continue
		}
		// Local fallback: build a proxyManager on demand (server-side injection may have
		// left it nil) and unpatch locally.
		if c.proxyManager == nil {
			c.proxyManager = newProxyManager(c.factory, c.clientset, c.ManagerNamespace)
		}
		local := make([]Resources, 0, len(workloads))
		for _, w := range workloads {
			local = append(local, Resources{Namespace: ns, Workload: w})
		}
		if err := c.proxyManager.Leave(ctx, local, ownerID); err != nil {
			return err
		}
	}

	plog.StepDone(ctx, "Removed proxy from %d workloads", len(resources))
	return nil
}
