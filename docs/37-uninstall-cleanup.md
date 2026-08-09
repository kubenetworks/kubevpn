# Uninstall & Cleanup

## 1. Overview

This document covers two related teardown paths:

- **`kubevpn uninstall`** — a deliberate, cluster-wide removal of *all* server-side KubeVPN
  resources from a namespace (the traffic manager Deployment, Service, ConfigMap, Secret, RBAC,
  Jobs, helper pod) plus the local Docker network.
- **Process-exit cleanup** (`ConnectOptions.Cleanup`) — the automatic, best-effort teardown a
  daemon runs when a connection ends or the daemon catches a termination signal. It is
  *connection-scoped* (releases leases, leaves proxies, runs rollbacks), not cluster-wide.

The two are complementary: cleanup unwinds one connection; uninstall removes the shared
infrastructure that survives across connections. Workload-level restore (leave/reset) is a third,
narrower concern — see [36-workload-restore.md](36-workload-restore.md).

## 2. Uninstall (`action.Uninstall`)

The `Uninstall` RPC builds a self-contained client from the request kubeconfig (no active
connection needed) and calls the package-level `Uninstall`, which deletes every resource by the
shared name `config.ConfigMapPodTrafficManager`, each with `GracePeriodSeconds=0`:

```
Uninstall(ctx, clientset, ns)                          pkg/daemon/action/uninstall.go
  ├── ConfigMaps   .Delete(kubevpn-traffic-manager)
  ├── Pods         .Delete(cni-net-dir-kubevpn)        ── the CIDR helper pod
  ├── Secrets      .Delete(...)
  ├── RoleBindings .Delete(...)
  ├── ServiceAccounts.Delete(...)
  ├── Roles        .Delete(...)
  ├── Services     .Delete(...)
  ├── Deployments  .Delete(...)                         ── the traffic manager itself
  ├── Jobs         .Delete(...)
  └── cleanupLocalContainer(ctx)                        ── remove local Docker network if empty
```

- Every delete error is **ignored** (`_ =`): uninstall is idempotent and must make progress even
  if some resources were already gone or partially created.
- `cleanupLocalContainer` inspects the `kubevpn-traffic-manager` Docker network and removes it only
  when it has no remaining containers (relevant to `kubevpn run`/`dev` — see
  [20-kubevpn-run.md](20-kubevpn-run.md)).
- The CLI `uninstall` command first `StartupDaemon` + `Disconnect` (best-effort) so the local VPN
  is torn down before the cluster resources disappear.

This is the inverse of the deployment described in
[24-traffic-manager-deployment.md](24-traffic-manager-deployment.md).

## 3. Exit Cleanup (`Cleanup`)

`Cleanup` runs from two triggers:

1. **Signal handler** — `DataSession.setupSignalHandler` (started by `DoConnect` in the root
   daemon) waits on `SIGHUP/SIGINT/SIGTERM/SIGQUIT` (`SIGKILL`/`SIGSTOP` are uncatchable and
   omitted) or `ctx.Done()`.
2. **Explicit disconnect/quit** call paths in `daemon/action`.

Cleanup dispatches by **type identity** — no `isDataPlane` flag or nil-check:

```
ConnectOptions.Cleanup(logCtx)                          pkg/handler/cleaner.go (user daemon)
  └── SessionBase.cleanup(logCtx, cleanupControlPlane)
        ├── cleanupMu.Lock; if cleanedUp ⇒ return      ── idempotent, but RETRYABLE
        ├── stop ConfigMapStore informer
        ├── ctx = WithTimeout(10s)
        ├── cleanupControlPlane(ctx)
        │        ├── delete helper pod + Jobs (grace=0)
        │        ├── LeaveAllProxyResources(ctx)        ── owner-scoped leave (see doc 36)
        │        ├── executeRollbackFuncs(...)
        │        └── on error: UNLOCK without setting cleanedUp ⇒ retried on next call
        └── cleanedUp = true; UNLOCK

DataSession.Cleanup(logCtx)                             pkg/handler/data_session.go (root daemon)
  └── SessionBase.cleanup(logCtx, cleanupDataPlane)
        ├── cleanupMu.Lock; if cleanedUp ⇒ return
        ├── stop ConfigMapStore informer
        ├── ctx = WithTimeout(10s)
        ├── cleanupDataPlane()
        │        ├── executeRollbackFuncs(...)
        │        ├── nm.Stop()                          ── tear down TUN/routes/DNS/port-forward
        │        └── cancel()
        └── cleanedUp = true; UNLOCK
```

### Why mutex + flag, not `sync.Once`

Cleanup uses a `cleanupMu` mutex and a `cleanedUp` bool rather than `sync.Once` **so it can be
retried**. If `cleanupControlPlane` fails (e.g. the API server is briefly unreachable), it unlocks
*without* setting `cleanedUp`, logs "Cleanup incomplete, will retry on next call", and a later
invocation tries again. `sync.Once` would permanently mark cleanup done after the first failed
attempt.

### Control plane vs data plane

This split mirrors the dual-daemon model ([02-dual-daemon.md](02-dual-daemon.md)):

| | Control plane (user daemon, `ConnectOptions`) | Data plane (root daemon, `DataSession`) |
|---|---|---|
| Type | `*ConnectOptions` (= `*ControlSession`) | `*DataSession` |
| Removes | helper pod, Jobs, proxy injections, rollbacks | rollbacks, NetworkManager, context |
| TUN IP | not its concern | **not explicitly released** — DHCP lease expiry reclaims it |

The type IS the discriminant — there is no `isDataPlane` field. Both types embed `SessionBase`
which provides the shared mutex-gated `cleanup` mechanics.

Note the TUN IP is intentionally *not* freed on cleanup; per the DHCP protocol the lease simply
expires and the reaper reclaims it — see [03-dhcp-ip-allocation.md](03-dhcp-ip-allocation.md).

## 4. Rollback Functions

Both cleanup paths run `executeRollbackFuncs(getRollbackFuncs())`. Components register undo
closures during setup via `ConnectOptions.AddRollbackFunc` (and `SyncOptions.AddRollbackFunc` for
sync). Each is invoked best-effort; errors are logged as warnings, never fatal. This is the
generic "undo what I did" hook (e.g. restoring a Service targetPort, removing an injected route).

## 5. Related Files

| File | Purpose |
|---|---|
| `pkg/daemon/action/uninstall.go` | `Uninstall` RPC + package `Uninstall` + `cleanupLocalContainer` |
| `cmd/kubevpn/cmds/uninstall.go` | CLI: disconnect then uninstall |
| `pkg/handler/cleaner.go` | `ConnectOptions.Cleanup`, `cleanupControlPlane`, `executeRollbackFuncs` |
| `pkg/handler/data_session.go` | `DataSession.Cleanup`, `cleanupDataPlane`, `setupSignalHandler` |
| `pkg/handler/session_base.go` | `SessionBase.cleanup` (shared mutex gate), `AddRollbackFunc`, `getRollbackFuncs` |

## 6. Related Docs

- [24-traffic-manager-deployment.md](24-traffic-manager-deployment.md) — the resources uninstall removes
- [36-workload-restore.md](36-workload-restore.md) — leave/reset (workload-scoped restore)
- [02-dual-daemon.md](02-dual-daemon.md) — control-plane vs data-plane split
- [03-dhcp-ip-allocation.md](03-dhcp-ip-allocation.md) — why TUN IP is reclaimed by lease expiry
- [12-session-lifecycle.md](12-session-lifecycle.md) — `SessionLifecycle` cleanup ordering
