# Fargate/Service Mode — Removed

The legacy Fargate/Service data plane has been removed. It injected SSH+Envoy
containers, modified `Service.targetPort`, and used SSH/reverse-UDP tunnels back
to the developer. That path no longer exists and there is no sidecar fallback.

`kubevpn proxy svc/<name>` now uses the same node-boundary agent as workload
proxying:

- the Service and backend PodSpecs are not modified;
- `Virtual.ServiceName` selects only the explicitly requested Service;
- direct listeners intercept ClusterIP/ServicePort traffic;
- matched traffic uses the developer's real TUN IP, including TCP and UDP;
- unmatched traffic returns explicitly to ClusterIP/ServicePort;
- leave removes only OwnerID rules and derived attachments.

This also means Service proxying now requires the privileged node-agent
DaemonSet. Unprivileged Fargate data planes are not supported.

Current architecture: [55-node-hosted-agent.md](55-node-hosted-agent.md).
