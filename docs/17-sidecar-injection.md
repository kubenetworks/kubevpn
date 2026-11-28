# Sidecar Injection — Removed

Workload sidecar injection has been removed. KubeVPN no longer creates VPN,
Envoy, or SSH sidecar containers in user workloads, does not patch controller
PodTemplates, and does not roll or recreate target pods.

All ordinary proxy, run, sync, and Service attachment now uses the node-boundary
DaemonSet. The manager publishes declarative `Virtual` and `NodeAttachment`
state; agents create direct listeners and explicit return routes. Legacy
sidecar-injected workloads are rejected rather than migrated.

The Syncthing helper in a sync-owned clone remains: it performs file
synchronization and is not a traffic-attachment sidecar. The traffic manager's
own `vpn` container also remains part of the manager, not a workload injection.

Current references:

- [55-node-hosted-agent.md](55-node-hosted-agent.md) — node-boundary data plane
- [26-sync-mode.md](26-sync-mode.md) — declarative original-to-clone sync routes
- [06-fargate-mode.md](06-fargate-mode.md) — Service migration from Fargate
