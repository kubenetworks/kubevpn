# Architecture Refactoring Review

## Overview

This document records the 2026-06-08 architecture refactoring session, the design decisions made, potential issues identified, and guidance for future work.

## What Changed

### Before

```
ConnectOptions (God Object — 30+ fields, 42 methods)
├── TUN device management
├── Port-forward lifecycle
├── Route table management
├── DNS configuration
├── Health checking + ConfigMap informer
├── Proxy workload inject/leave
├── DHCP IP allocation
├── Session lifecycle
└── Persistence serialization
```

### After

```
ConnectOptions (Session Orchestrator — 15 fields, ~20 methods)
├── K8sClient (embedded)
├── Configuration (ManagerNamespace, WorkloadNamespace, Image, etc.)
├── Session lifecycle (ctx, cancel, DHCP, rollback)
├── network *NetworkManager         ← owns TUN + routes + DNS + port-forward
├── proxyManager *ProxyManager      ← owns sidecar inject/leave
├── configMapStore *ConfigMapStore  ← owns ConfigMap + health check
└── Sync *SyncOptions
```

### Dependency Cleanup

| Package | Old deps | New deps |
|---------|----------|----------|
| `pkg/util` | daemon/rpc, cp | config only |
| `pkg/ssh` | daemon/rpc | config, util only |
| `pkg/handler` | daemon/rpc (ConnectRequest stored) | No rpc types in core structs |

### File Splits

| File | Before | After |
|------|--------|-------|
| `daemon/handler/ssh.go` | 434 lines | 197 (+ registry.go 48, installer.go 109, tunnel.go 115) |
| `handler/network.go` | N/A (new) | 645 lines — full network lifecycle |
| `handler/proxy_manager.go` | N/A (new) | ~110 lines |
| `handler/configmap_store.go` | N/A (new) | ~100 lines |

---

## Potential Issues Identified

### 1. ConfigMapStore nil pointer (LOW risk)

**Scenario:** If `Get()`, `Set()`, `HealthCheckOnce()` etc. are called before `InitClient()` succeeds, `c.configMapStore` will be nil.

**Why it's LOW risk:** All code paths call `InitClient` first. The daemon/action layer always creates ConnectOptions then immediately calls `InitClient`. Tests create ConfigMapStore in the test helper.

**Mitigation (future):** Add defensive nil check in delegating methods if ConnectOptions is ever used in contexts where InitClient might not be called.

### 2. Informer goroutine lifecycle

**Scenario:** `ConfigMapStore` starts a goroutine via `go c.informer.Run(c.informerStop)`. If `Stop()` is never called (process crash, panic), the goroutine leaks until process exit.

**Why it's acceptable:** kubevpn is a CLI/daemon tool — process exit cleans up all goroutines. The informer is stopped in `Cleanup()` which is called on disconnect, quit, and signal handling.

### 3. NetworkManager.Stop() ordering

**Scenario:** `cleanupDataPlane` calls `nm.Stop()` which cancels the network context. But `executeRollbackFuncs` runs BEFORE `nm.Stop()`. If a rollback function needs the network (e.g., to remove a resource via the TUN), it might already be broken.

**Current code:**
```go
func (c *ConnectOptions) cleanupDataPlane(logCtx context.Context) {
    executeRollbackFuncs(logCtx, c.getRollbackFuncs())  // runs first
    if c.network != nil {
        c.network.Stop()                                 // then stop network
    }
    if c.cancel != nil {
        c.cancel()
    }
}
```

**Status:** This ordering is CORRECT — rollback functions (like removing routes) need the network to still be up. The network is stopped AFTER rollbacks complete.

### 4. Persistence serialization unchanged

**Verified:** Fields with `json:` tags that survive persistence are:
- `RequestRaw []byte` — protobuf-serialized ConnectRequest
- `OwnerID string` — connection ownership
- `LocalTunIPv4 *net.IPNet` — leased IPv4
- `LocalTunIPv6 *net.IPNet` — leased IPv6

Removed fields (`cidrs`, `apiServerIPs`, `tunName`, `dnsConfig`, `extraHost`, `healthStatus`, `cmInformer*`) never had json tags and were never persisted. **No persistence breakage.**

### 5. Dead code removed

The `addRoute` wrapper on ConnectOptions was dead code after NetworkManager took over `portForwardOnce`. Confirmed by build passing after deletion — no remaining callers.

### 6. ExtraRouteInfo shared by pointer

**Design:** `NetworkConfig.ExtraRouteInfo` is `*ExtraRouteInfo` (pointer to ConnectOptions' field). `AddExtraNodeIP()` mutates `ExtraCIDR` on this pointer, which is then read by `startTUN()`.

**Risk:** If ConnectOptions and NetworkManager run concurrently on different goroutines accessing ExtraRouteInfo, there could be a data race. Currently safe because `Start()` is synchronous and `AddExtraNodeIP` completes before `startTUN` reads ExtraCIDR.

**Future consideration:** If `Start()` becomes concurrent or if hot-reload re-reads ExtraRouteInfo, add a copy or mutex.

### 7. sort.go accesses NetworkManager internal config

```go
var aAPIServerIPs []net.IP
if a.network != nil {
    aAPIServerIPs = a.network.cfg.APIServerIPs
}
```

This accesses `cfg` (an exported field of NetworkManager) directly. Not ideal for encapsulation, but pragmatic — adding a getter for a sort comparator would be over-engineering.

---

## Hot-Reload Design (Future)

NetworkManager was designed to support hot-reload:

```go
// Future API
func (nm *NetworkManager) Restart(ctx context.Context) error {
    nm.Stop()
    return nm.Start(ctx)
}
```

### What works today:
- `Stop()` cancels nm.ctx → stops port-forward retries, informers, DNS
- `Start()` is idempotent from a fresh state (all runtime fields cleared in Stop)

### What needs work for hot-reload:
1. **Route table cleanup** — Stop() doesn't remove routes added to the OS. Need a `tun.RemoveRoutes(nm.tunName, ...)` call.
2. **TUN device teardown** — Stop() cancels the context which closes the listener, but the OS TUN device may persist. Need explicit `tun.Delete(nm.tunName)`.
3. **DNS restore** — `dns.Config.CancelDNS()` restores `/etc/resolv.conf` but may leave stale hosts entries.
4. **Port re-allocation** — New gvisor ports are allocated on each Start(). If the traffic manager pod didn't restart, it might still expect the old ports. The port-forward retry loop handles this naturally.

### Recommended hot-reload sequence:
```
1. nm.Stop()           — cancel ctx, stop DNS, close TUN
2. tun.Delete(tunName) — explicitly remove OS TUN device  
3. nm.Start(newCtx)    — re-allocate ports, new TUN, new routes, new DNS
```

---

## Testing Notes

- `go test ./pkg/handler/...` passes (18.7s)
- `go build ./...` passes
- `go vet ./pkg/...` passes (ignoring pre-existing copylocks warnings)
- Daemon action tests pass: `go test ./pkg/daemon/action/...`

---

## Naming Conventions (Updated)

| Field | Meaning |
|-------|---------|
| `ManagerNamespace` / `managerNamespace` | Traffic manager namespace |
| `WorkloadNamespace` / `workloadNamespace` | User workload namespace |
| RPC `req.Namespace` | Always workload namespace |
| RPC `req.ManagerNamespace` | Always manager namespace |
