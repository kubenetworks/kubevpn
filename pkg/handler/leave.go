package handler

import (
	"context"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// LeaveAllProxyResources removes all proxy sidecar injections for this connection's
// OwnerID. It is called on disconnect/cleanup. With server-side injection the client no
// longer tracks which mesh workloads it proxied, so it asks the manager to remove every
// rule for its OwnerID in the workload namespace (empty Workloads = leave-all). It also
// stops any local Service Mapper. Best-effort: on a clean disconnect the VPN is still up
// so this reaches the manager; if it fails (e.g. VPN already torn down / crash), the
// manager's lease reaper reclaims the abandoned rules later (see docs/44 §3b), so a
// failure here is logged but does not block teardown.
func (c *ConnectOptions) LeaveAllProxyResources(ctx context.Context) error {
	if c == nil || c.clientset == nil {
		return nil
	}
	if err := c.leaveViaManager(ctx, c.WorkloadNamespace, nil, c.OwnerID); err != nil {
		plog.G(ctx).Warnf("Server-side leave-all failed (rules will be reclaimed by the manager's lease reaper): %v", err)
	}
	if c.proxyManager != nil {
		c.proxyManager.StopAll()
	}
	return nil
}

// LeaveResource unpatches the given proxy resources server-side: the traffic manager
// removes the envoy rule and (when no rule remains) the sidecar with its own
// ServiceAccount over the VPN. There is no local fallback. The workloads come from the
// request (not a client-side record, which no longer exists for mesh workloads under
// server-side injection). It also stops any local Service Mapper for the left workloads.
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
		if err := c.leaveViaManager(ctx, ns, workloads, ownerID); err != nil {
			return err
		}
		// Manager did the cluster writes; stop any local Mapper for these workloads.
		c.stopServiceMappers(ns, workloads)
	}

	plog.StepDone(ctx, "Removed proxy from %d workloads", len(resources))
	return nil
}
