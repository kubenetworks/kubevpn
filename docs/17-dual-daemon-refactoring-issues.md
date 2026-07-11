# Dual-Daemon Architecture: Refactoring Issues & Design Notes

## Background

KubeVPN uses two daemon processes:

| Daemon | Process | Role | ConnectOptions usage |
|--------|---------|------|---------------------|
| User Daemon | `kubevpn daemon` | DHCP, proxy inject, health check, persist config | Control plane — `isDataPlane=false` |
| Root Daemon | `kubevpn daemon --sudo` | TUN device, port-forward, routes, DNS | Data plane — `isDataPlane=true` |

Both instantiate `ConnectOptions` but activate different subsets of functionality.

## Initialization Flow Comparison

### Root Daemon (connect.go — IsSudo=true)

```
1. ConnectOptions created with ManagerNamespace = req.ManagerNamespace (correct from CLI)
2. InitClient(ManagerNamespace) → K8sClient initialized
3. connect.OwnerID = req.OwnerID (from ConnectRequest proto)
4. DoConnect() →
   a. getCIDR (detect CIDRs)
   b. getCIDR → getConfigMapStore() → created with CORRECT ManagerNamespace
   c. NetworkManager.Start() → portForward, startTUN (含 rentIP), routes, DNS
5. Stored in svr.connections
```

### User Daemon (connect_elevate.go — IsSudo=false)

```
1. ConnectOptions created with ManagerNamespace = req.Namespace (WORKLOAD namespace — temporary!)
2. InitClient(req.Namespace) → K8sClient initialized
3. detectAndSetManagerNamespace() → MAY CHANGE connect.ManagerNamespace
4. forwardConnectToSudo() → set req.OwnerID, stream connect request to root daemon
7. HealthCheckOnce() → getConfigMapStore() → created with CURRENT ManagerNamespace
8. HealthPeriod() → periodic check loop
9. Stored in svr.connections + OffloadToConfig()
```

## Regression Found & Fixed

### Problem: ConfigMapStore Created Too Early

The initial refactoring created `configMapStore` eagerly in `InitClient()`:
```go
func (c *ConnectOptions) InitClient(f cmdutil.Factory) error {
    c.ManagerNamespace, err = c.K8sClient.InitClient(f)
    c.configMapStore = NewConfigMapStore(c.clientset, c.ManagerNamespace) // ← BUG
    return err
}
```

In the user daemon path:
1. `InitClient(req.Namespace)` → configMapStore created with **workload namespace**
2. `detectAndSetManagerNamespace()` → changes `connect.ManagerNamespace` to actual manager ns
3. `HealthCheckOnce()` → uses configMapStore which has **stale namespace**

The health check would query the ConfigMap in the wrong namespace.

### Fix: Lazy Initialization

Changed to lazy creation — configMapStore is created on first access, by which point `ManagerNamespace` is finalized:

```go
func (c *ConnectOptions) getConfigMapStore() *ConfigMapStore {
    if c.configMapStore == nil {
        c.configMapStore = NewConfigMapStore(c.clientset, c.ManagerNamespace)
    }
    return c.configMapStore
}
```

The old code (pre-refactoring) was safe because it used `sync.Once` on `GetConfigMapInformer()` — first call happened AFTER namespace was finalized. The new design preserves this invariant through lazy initialization.

## Sub-Manager Activation by Daemon Role

After refactoring, each sub-manager is only active in one daemon:

| Sub-manager | User Daemon | Root Daemon | Notes |
|-------------|-------------|-------------|-------|
| `network *NetworkManager` | nil | Start()/Stop() | Only root creates TUN/routes/DNS |
| `proxyManager *ProxyManager` | Created on proxy | nil | Only user daemon injects sidecars |
| `configMapStore *ConfigMapStore` | Created on health check | Created on getCIDR | Both need ConfigMap access |
| TunConfigService | N/A (查询 sudo daemon IP) | rentIP in startTUN | Root daemon allocates IP |

## Cleanup Path Differences

```go
func (c *ConnectOptions) Cleanup(logCtx context.Context) {
    c.once.Do(func() {
        if c.configMapStore != nil { c.configMapStore.Stop() }

        if !c.isDataPlane {
            c.cleanupControlPlane(logCtx, ctx)  // User daemon
        } else {
            c.cleanupDataPlane(logCtx)          // Root daemon
        }
    })
}
```

### User Daemon (cleanupControlPlane):
1. (无显式 IP 释放 — lease 过期自动回收，遵循 DHCP 协议设计)
2. Delete outbound pod / job
3. Leave all proxy resources (unpatch sidecars)
4. Run rollback functions

### Root Daemon (cleanupDataPlane):
1. Run rollback functions (route cleanup, etc.)
2. NetworkManager.Stop() (cancel DNS, port-forward, TUN)
3. Cancel session context

## Invariants That Must Hold

1. **configMapStore must not be created before ManagerNamespace is finalized.** Enforced by lazy `getConfigMapStore()`. First access in user daemon is `HealthCheckOnce` (after `detectAndSetManagerNamespace`). First access in root daemon is `getCIDR` (ManagerNamespace is correct from start).

2. **NetworkManager is only created in root daemon.** `DoConnect()` sets `isDataPlane=true` and creates NetworkManager. User daemon never calls `DoConnect()`.

3. **ProxyManager is only created in user daemon.** `CreateRemoteInboundPod()` is only called by `daemon/action/proxy.go` which runs in user daemon context. Root daemon never proxies.

4. **Cleanup must be idempotent.** `sync.Once` ensures Cleanup runs exactly once even if called from signal handler and explicit disconnect concurrently.

5. **Persistence only serializes OwnerID + RequestRaw.** TUN IP 不再持久化在 ConnectOptions 中（已移至 NetworkManager）。重启时 Root Daemon 通过 OwnerID 从 TunConfigService 重新获取 IP。User Daemon 通过 `getSudoTunIPs` 查询。

## Potential Future Issues

### 1. ManagerNamespace Drift After configMapStore Creation

If code is added that calls `getConfigMapStore()` BEFORE `detectAndSetManagerNamespace` in the user daemon path, the store will be created with the wrong namespace. To guard against this:
- Consider adding an assertion: `if c.configMapStore != nil && c.configMapStore.managerNamespace != c.ManagerNamespace { panic }` in debug builds.
- Or: make `getConfigMapStore()` always check and recreate if namespace changed.

### 2. Concurrent Access to ConnectOptions

Both daemons store `*ConnectOptions` in `svr.connections`. The user daemon's `HealthPeriod` goroutine reads `configMapStore` while the main goroutine might call `Cleanup`. This is safe because:
- `HealthPeriod` checks `ctx.Done()` and exits
- `Cleanup` calls `configMapStore.Stop()` which closes the informer channel
- Concurrent `HealthCheckOnce` after `Stop()` will just get an error from the K8s API (informer stopped)

### 3. ExtraRouteInfo Pointer Sharing (Root Daemon Only)

`NetworkConfig.ExtraRouteInfo` is a pointer to ConnectOptions' field. In root daemon, `AddExtraNodeIP()` mutates `ExtraCIDR` on this pointer. This is safe because `Start()` is synchronous. But if hot-reload is implemented, concurrent access could race.

### 4. Connection Restore After Daemon Restart

`LoadFromConfig` deserializes `ConnectOptions` from JSON and replays the connect request. The deserialized struct has nil sub-managers — they get rebuilt as the replayed connect flows through the normal path. Persistence stores OwnerID + RequestRaw. On restart:
- OwnerID is preserved → TunConfigService returns the same IP (if lease not expired)
- If lease expired → TunConfigService allocates a new IP → client uses new IP seamlessly
