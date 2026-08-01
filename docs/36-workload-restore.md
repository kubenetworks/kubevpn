# Workload Restore (Leave & Reset)

## 1. Overview

When KubeVPN proxies a workload it mutates the cluster: it injects sidecar containers
(`vpn`/`envoy-proxy`/…), adds envoy routing rules to the traffic manager ConfigMap, and may
rewrite a Service's `targetPort`. Two commands undo this:

- **`kubevpn leave`** — *ownership-scoped* removal. Removes **only the current connection's**
  envoy rules (matched by `OwnerID`). The sidecar and Service rewrite are removed only if **no
  other owner** still proxies the workload. This is the normal, multi-user-safe path.
- **`kubevpn reset`** — *unconditional* full restoration. Removes **all** envoy rules for the
  named workloads regardless of owner, strips the sidecars, and restores the Service. This is the
  recovery hammer for when state is inconsistent (e.g. a crashed client left a workload patched).

Both are control-plane operations (they edit cluster resources, not the TUN). See
[02-dual-daemon.md](02-dual-daemon.md). They share the lower-level primitives in `pkg/inject`
documented in [17-sidecar-injection.md](17-sidecar-injection.md).

## 2. Leave (ownership-scoped)

```
RPC Leave (action/leave.go)
  └── findConnection(currentConnectionID)            ── needs an active connection
      └── conn.LeaveResource(ctx, resources, conn.OwnerID)        handler/leave.go
            └── ProxyManager.Leave(resources, ownerID)            handler/proxy_manager.go
                  for each workload:
                    ├── GetTopOwnerObject  ── walk to the controlling Deployment/StatefulSet/…
                    ├── nodeID = "<groupResource>.<name>"
                    ├── inject.UnpatchContainer(nodeID, …, ownerID)            inject/mesh.go
                    │     ├── removeEnvoyConfig(ns, nodeID, ownerID)  ── drop THIS owner's rules
                    │     ├── !found  ⇒ nothing to do
                    │     ├── !empty  ⇒ other owners remain ⇒ KEEP sidecar, return
                    │     └── empty   ⇒ RemoveContainers + patchWorkload  ── strip sidecars
                    ├── if empty && IsK8sService ⇒ RestoreServiceTargetPort
                    └── ProxyManager.Remove(ns, workload)          ── drop from in-memory list
```

Key points:
- `ProxyManager` tracks the connection's proxied workloads in memory (`workloads ProxyList`,
  mutex-guarded). `LeaveAllProxyResources` / `LeaveAll` operate over that whole list (used during
  disconnect/cleanup); `LeaveResource` / `Leave` operate over an explicit subset (the `leave`
  command).
- `UnpatchContainer` returns `empty bool` — *true* means the ConfigMap has no remaining rules for
  that workload after this owner's rules were dropped, which gates the destructive steps
  (container removal + Service restore). This is what makes leave safe when multiple developers
  proxy the same workload — see [05-owner-id.md](05-owner-id.md).
- Errors are aggregated and wrapped with `config.ErrCleanupFailed`.

## 3. Reset (unconditional)

```
RPC Reset (action/reset.go)
  ├── resolveKubeconfig(SshJump, KubeconfigBytes, false)   ── no active connection required
  ├── ConnectOptions.InitClient + DetectManagerNamespace
  └── connect.Reset(ns, workloads)                         handler/reset.go
        ├── util.NormalizedResource           ── expand/normalize workload names
        ├── resetConfigMap(managerNS, ns, workloads)
        │     ├── read ConfigMap ENVOY_CONFIG (KeyEnvoy)
        │     ├── unmarshal []*controlplane.Virtual
        │     ├── drop every Virtual whose UID ∈ workloads AND Namespace == ns   (ALL owners)
        │     └── write back
        └── for each workload: removeInjectContainer(ns, workload)
              ├── GetTopOwnerObject + GetPodTemplateSpecPath  ── find template + json-path depth
              ├── inject.RemoveContainers(&templateSpec.Spec)  ── strip sidecars unconditionally
              ├── JSONPatch replace /<depth>/spec  via resource.Helper.Patch
              ├── util.RolloutStatus  ── wait for rollout (warn-only)
              └── if IsK8sService ⇒ inject.RestoreServiceTargetPort
```

Reset differs from leave in three ways:

| | Leave | Reset |
|---|---|---|
| Scope | this `OwnerID` only | all owners of the named workloads |
| Needs active connection | yes (`findConnection`) | no (builds its own client from kubeconfig) |
| Sidecar removal | only when last owner leaves | always |
| Rule matching | `removeEnvoyConfig(nodeID, ownerID)` | UID + namespace match in `ENVOY_CONFIG` |

## 4. Shared Primitives (`pkg/inject`)

| Primitive | What it does |
|---|---|
| `RemoveContainers(spec)` | Deletes containers whose name ∈ `sidecarNames` (vpn/envoy/dns/…) from the pod spec |
| `UnpatchContainer(nodeID, …, ownerID)` | Owner-scoped rule removal; returns whether the workload is now rule-empty |
| `removeEnvoyConfig(ns, nodeID, ownerID)` | Drops one owner's rules from `ENVOY_CONFIG`; returns `(empty, found, err)` |
| `RestoreServiceTargetPort(clientset, ns, name)` | Reverts a Service's rewritten `targetPort` (fargate/service mode) |
| `JSONPatchOp` + `resource.Helper.Patch` | Applies the template-spec replacement at the correct owner depth |

`GetTopOwnerObject` is critical to both paths: the patch must be applied to the *top* controller
(Deployment), not the Pod, so the controller does not immediately re-inject the old spec.

## 5. Edge Cases

- **No ConfigMap / no `ENVOY_CONFIG`** ⇒ both paths treat it as "nothing to do" (no error).
- **Rollout wait failure** is warn-only — the spec patch already succeeded; a slow rollout should
  not fail the command.
- **Service vs non-Service** — `RestoreServiceTargetPort` only runs for `svc/…` targets
  (fargate/service mode); see [06-fargate-mode.md](06-fargate-mode.md).
- Full teardown of the traffic manager itself (uninstall) and process-exit cleanup are a separate
  concern — see [37-uninstall-cleanup.md](37-uninstall-cleanup.md).

## 6. Related Files

| File | Purpose |
|---|---|
| `pkg/handler/leave.go` | `LeaveAllProxyResources`, `LeaveResource` |
| `pkg/handler/proxy_manager.go` | `ProxyManager.Leave/LeaveAll`, in-memory workload tracking |
| `pkg/handler/reset.go` | `Reset`, `resetConfigMap`, `removeInjectContainer` |
| `pkg/daemon/action/leave.go` | `Leave` RPC (needs active connection) |
| `pkg/daemon/action/reset.go` | `Reset` RPC (self-contained client) |
| `pkg/inject/mesh.go` | `UnpatchContainer`, `removeEnvoyConfig` |
| `pkg/inject/container.go` | `RemoveContainers`, sidecar name set |
| `pkg/inject/fargate.go` | `RestoreServiceTargetPort` |

## 7. Related Docs

- [17-sidecar-injection.md](17-sidecar-injection.md) — the injection these commands undo
- [05-owner-id.md](05-owner-id.md) — how `OwnerID` scopes leave
- [06-fargate-mode.md](06-fargate-mode.md) — Service targetPort rewrite/restore
- [37-uninstall-cleanup.md](37-uninstall-cleanup.md) — traffic-manager teardown & exit cleanup
