# Dual Daemon Architecture

## Why This Document Exists

KubeVPN uses a dual-process architecture (User Daemon + Root Daemon), where each process holds an **independent session instance** with completely different field semantics and usage scenarios. After the C2-A refactoring, the two session types are:

- **`ConnectOptions`** (= `ControlSession`) — User Daemon. Manages traffic manager, proxy injection, persistence, and the per-connection user-daemon-managed local SOCKS5 proxy (`connect --socks`, started in `forwardConnectToSudo`).
- **`DataSession`** — Root Daemon. Manages TUN device, routing, DNS, and the NetworkManager lifecycle.

Both types implement the `Connection` interface. Both embed `SessionBase` (K8s client bundle + rollback lifecycle). They do NOT share memory across daemon boundaries.

In the past, the lack of this architecture document led to bugs where fields were initialized in the wrong daemon (e.g., OwnerID being generated uselessly in the Root Daemon).

**Anyone modifying session type fields or `daemon/action/` code must read this document first.**

---

## 1. Architecture Overview

```
┌─────────────────────────────────────────────────────┐
│                     User CLI                         │
│              cmd/kubevpn/cmds/                       │
└──────────────────────┬──────────────────────────────┘
                       │ gRPC (unix socket)
                       ▼
┌─────────────────────────────────────────────────────┐
│              User Daemon (unprivileged, no root)      │
│              pkg/daemon/action/ (IsSudo=false)        │
│                                                       │
│  Responsibilities:                                    │
│  ├── SSH jump host (resolveKubeconfigBytes)           │
│  ├── Traffic Manager creation/upgrade (CreateOutboundPod/UpgradeDeploy)│
│  ├── Proxy injection (CreateRemoteInboundPod → inject/)│
│  ├── File sync (DoSync)                               │
│  ├── Connection state persistence (OffloadToConfig/LoadFromConfig)│
│  └── OwnerID generation and management                │
│                                                       │
│  Holds: its own ConnectOptions (control plane)         │
│  Storage: svr.connections slice                        │
└──────────────────────┬──────────────────────────────┘
                       │ gRPC (unix socket, sudo)
                       ▼
┌─────────────────────────────────────────────────────┐
│              Root Daemon (root privileges)             │
│              pkg/daemon/action/ (IsSudo=true)         │
│                                                       │
│  Responsibilities:                                    │
│  ├── TUN device creation and management               │
│  ├── Routing table operations (addRoute, addRouteDynamic)│
│  ├── DNS configuration (setupDNS, CancelDNS)          │
│  ├── Port forwarding (portForward to traffic manager pod)│
│  ├── gvisor network stack operation                    │
│  └── Signal handling and cleanup                      │
│                                                       │
│  Holds: its own DataSession (data plane)               │
│  Storage: svr.connections slice                        │
└─────────────────────────────────────────────────────┘
```

## 2. Session Type Separation (C2-A)

**Key rule: The User Daemon and Root Daemon each create independent session instances. They do NOT share memory.**

After the C2-A refactoring:
- User Daemon holds `*handler.ConnectOptions` (= `*handler.ControlSession`, a type alias).
- Root Daemon holds `*handler.DataSession`.
- Both implement `handler.Connection` and embed `handler.SessionBase`.

### 2.1 User Daemon: ConnectOptions / ControlSession

Creation site: `daemon/action/connect_elevate.go` → `redirectConnectToSudoDaemon()`

```go
connect := &handler.ConnectOptions{
    ManagerNamespace:     req.Namespace,
    WorkloadNamespace:    req.Namespace,
    ExtraRouteInfo:       ...,
    OriginKubeconfigPath: req.OriginKubeconfigPath,
    RequestRaw:           reqBytes,
    OwnerID:              uuid.New().String()[:12],  // ← generated here only
    Image:                req.Image,                 // ← used by CreateOutboundPod
    ImagePullSecretName:  req.ImagePullSecretName,   // ← used by CreateOutboundPod
}
```

Fields used:
| Field | Purpose |
|-------|---------|
| `SessionBase.K8sClient` (clientset, factory) | Interact with K8s API (inject sidecar, query ConfigMap) |
| `OwnerID` | Written into Envoy Rule to identify ownership |
| `Image/ImagePullSecretName` | CreateOutboundPod creates the traffic manager pod |
| `proxyManager` | ProxyManager — manages proxied workloads lifecycle |
| `Sync` | File sync options |
| `RequestRaw` | Needed for persistence |

**Stubs (always return zero/nil/error on ConnectOptions)**:
| Method | Reason |
|--------|--------|
| `DoConnect()` | Returns error — data-plane DoConnect is on DataSession |
| `Context()` | Returns nil — no data-plane ctx in user daemon |
| `GetLocalTunIP()` | Returns "", "" — TUN IP is in DataSession |
| `GetLastHeartbeat()` | Returns time.Time{} — NetworkManager is in DataSession |
| `GetAPIServerIPs()` | Returns nil — NetworkManager is in DataSession |
| `GetNetworkExtraHost()` | Returns nil — NetworkManager is in DataSession |

### 2.2 Root Daemon: DataSession

Creation site: `daemon/action/connect.go` → `Connect()` (IsSudo=true branch)

```go
ds := &handler.DataSession{
    SessionBase: handler.SessionBase{K8sClient: handler.K8sClient{}},
    ManagerNamespace:     req.ManagerNamespace,
    ExtraRouteInfo:       ...,
    OriginKubeconfigPath: req.OriginKubeconfigPath,
    WorkloadNamespace:    req.Namespace,
    Lock:                 &svr.Lock,
    Image:                req.Image,
    ImagePullSecretName:  req.ImagePullSecretName,
    OwnerID:              req.OwnerID,
    ConnectionID:         req.ConnectionID,
    ReservedTunIPs:       svr.siblingTunIPs,
}
```

Fields used:
| Field | Purpose |
|-------|---------|
| `ctx/cancel` | Set at START of DoConnect; controls data-plane lifecycle |
| `nm` | NetworkManager: TUN, port forwarding, routes, DNS |
| `Lock` | Shared DNS lock from svr.Lock |
| `ReservedTunIPs` | Sibling TUN IPs to exclude from allocation |
| `SessionBase.configMapStore` | ConfigMap informer for CIDR caching |

`DataSession.Cleanup()` is ALWAYS the data-plane path — no nil-check needed.
`DataSession` is NEVER persisted — `OffloadToConfig` only type-asserts to `*ConnectOptions`.

**Stubs (always return zero/nil on DataSession)**:
| Method | Reason |
|--------|--------|
| `CreateRemoteInboundPod()` | Returns error — proxy injection is in ConnectOptions |
| `LeaveAllProxyResources()` | Returns nil (safe no-op) |
| `LeaveResource()` | Returns nil (safe no-op) |
| `ProxyResources()` | Returns nil |
| `GetSync()` | Returns nil |
| `SetSync()` | No-op |

### 2.3 ManagerNamespace Lock-Step Invariant

`ManagerNamespace` is the one control-plane field the Root Daemon also needs (for
port-forward, TLS secret, CIDR detection — see [07-namespace-model.md](07-namespace-model.md)).
The two daemons must **never disagree** on it.

Resolution happens in the **User Daemon only**, via `detectAndSetManagerNamespace`
(`connect_elevate.go`). The Root Daemon never runs detection — it trusts the value
forwarded on the request. The invariant is enforced by writing the resolved namespace to
**both** sides in a single shared assignment:

```go
// detectAndSetManagerNamespace (User Daemon)
if req.ManagerNamespace == "" {
    req.ManagerNamespace, _ = util.DetectManagerNamespace(ctx, connect.GetFactory(), req.Namespace)
}
if req.ManagerNamespace == "" {
    req.ManagerNamespace = req.Namespace            // fallback: create in workload ns
}
connect.ManagerNamespace = req.ManagerNamespace     // ← user side == forwarded side, always
```

`forwardConnectToSudo` then sends this same `req` to the Root Daemon, which assigns
`connect.ManagerNamespace = req.ManagerNamespace`. Because the User Daemon's
`connect.ManagerNamespace` and the forwarded `req.ManagerNamespace` come from the **same**
trailing assignment, they are identical by construction — the `ConnectionID` (derived from
the manager namespace UID) and all data-plane operations therefore agree across daemons.

> Do not set `connect.ManagerNamespace` only inside the "detected" branch and rely on the
> struct's initial `ManagerNamespace: req.Namespace` to cover the fallback branch — that
> implicit coupling lets the two sides diverge if the initializer ever changes. Keep the
> assignment explicit and shared. Invariant test: `TestDetectAndSetManagerNamespace_UserRootConsistency`.

## 3. Operation Flows

### 3.1 Connect Flow

```
CLI: kubevpn connect
  │
  ▼
User Daemon: Connect RPC
  ├── redirectConnectToSudoDaemon()
  │     ├── Create ConnectOptions (control plane, with OwnerID, Image)
  │     ├── resolveKubeconfigBytes (SSH jump host → in-memory bytes, no temp file)
  │     ├── InitClient (InitFactoryByBytes)
  │     ├── detectAndSetManagerNamespace
  │     ├── forwardConnectToSudo(..., resolvedBytes)
  │     │     ├── CreateOutboundPod (create traffic manager pod)
  │     │     ├── UpgradeDeploy (upgrade traffic manager)
  │     │     ├── Set req.KubeconfigBytes = resolvedBytes (no os.ReadFile round-trip)
  │     │     ├── Set req.OwnerID, forward req to Root Daemon ──┐
  │     │     ├── Wait for Root Daemon to complete               │
  │     │     └── Store in svr.connections                       │
  │                                                              ▼
  │                                    Root Daemon: Connect RPC
  │                                      ├── Idempotency guard: if a DataSession already
  │                                      │   exists for req.ConnectionID, return success
  │                                      │   without rebuilding (see note below)
  │                                      ├── Create DataSession (data plane)
  │                                      ├── ds.OwnerID = req.OwnerID
  │                                      ├── ds.DoConnect()
  │                                      │     ├── getCIDR
  │                                      │     ├── NetworkManager.Start()
  │                                      │     │     ├── portForward
  │                                      │     │     ├── startTUN (includes rentIP to allocate TUN IP)
  │                                      │     │     ├── AddRouteDynamic
  │                                      │     │     └── setupDNS
  │                                      │     └── StartIPWatcher
  │                                      └── Store in svr.connections
  ▼
User Daemon: Return success to CLI
```

> **Root-daemon Connect is idempotent per ConnectionID.** The user-side dedup in
> `redirectConnectToSudoDaemon` only checks the user daemon's in-memory `svr.connections`, which
> is empty after a user-daemon restart. So when `LoadFromConfig` replays `Connect` while the root
> daemon is still running, the root side must also short-circuit on a known `ConnectionID` — else
> it would build a second DataSession (a duplicate TUN / port-forward / route / DNS setup) and
> leak. Guard: `Connect` (IsSudo) → `svr.findConnection(req.ConnectionID)` before creating `ds`.

### 3.1a Peer Liveness (user ↔ root) and crash recovery

After `Connect` returns there is **no long-lived stream** between the two daemons — the data
plane lives on the root daemon's own `ds.ctx`, and each daemon's `detectUnixSocksFile` watchdog
only checks its *own* socket. The user daemon therefore runs `MonitorSudoLiveness` (every
`config.SudoLivenessProbeInterval` = 2s): a crash-safe PID pre-check + bounded gRPC probe of the
root daemon, caching a per-connection health snapshot that `Status`/`ConnectionList` serve. See
[08-heartbeat-health.md](08-heartbeat-health.md) #6.

**Recovery is report-only.** A crashed root daemon is reflected as `unhealthy`/`disconnected`
within ~2s, but the user daemon does **not** respawn it: spawning the root daemon needs
interactive privilege escalation ([34-privilege-escalation.md](34-privilege-escalation.md)), which
a detached background daemon has no TTY for. Re-launch happens on the next CLI command's
`StartupDaemon` (foreground). At that startup the user daemon also runs `ReconcileSudoConnections`
to reap orphaned data-plane sessions left by daemon drift.

### 3.2 Proxy Flow (User Daemon only)

```
CLI: kubevpn proxy deploy/web --headers version=v1
  │
  ▼
User Daemon: Proxy RPC
  ├── Connect (same flow as above)
  ├── findConnection(connectionID)  ← finds the User Daemon's ConnectOptions
  └── CreateRemoteInboundPod()
        ├── inject.NewInjector(InjectOptions{OwnerID: c.OwnerID})
        │     └── addEnvoyConfig(..., ownerID)
        │           └── Rule{OwnerID: ownerID}  ← written to ConfigMap
        └── Start Mapper (SSH tunnel)
```

### 3.3 Disconnect Flow

```
CLI: kubevpn disconnect
  │
  ▼
User Daemon: Disconnect RPC
  ├── Forward to Root Daemon ──────────────┐
  │                                        ▼
  │                            Root Daemon: Disconnect RPC
  │                              └── cleanupDataPlane()
  │                                    ├── rollback functions
  │                                    ├── CancelDNS
  │                                    └── cancel context → TUN/routes stop
  │
  └── cleanupControlPlane()
        ├── Delete temporary Pod/Job (CNI pod, Helm job)
        ├── LeaveAllProxyResources (clean up Envoy Rules, matched by OwnerID)
        ├── rollback functions
        └── Note: TUN IP is NOT explicitly released — DHCP lease expiry handles reclaim
```

## 4. Rules for Modifying Session Types

### Rule 1: Determine which daemon a new field belongs to before adding it

Ask yourself: is this field used by `DoConnect` (data plane), or by `CreateRemoteInboundPod`/`LeaveResource` (control plane)?

- **Data plane** → add to `DataSession`: does not need a json tag, does not need persistence.
- **Control plane** → add to `ConnectOptions`: needs a json tag (for persistence), needs to be initialized in `redirectConnectToSudoDaemon`.
- **Shared** (both daemons read from ConnectRequest) → add to `SessionBase` only if it is NOT set in composite literals by test code. Identity fields (ManagerNamespace, OwnerID, etc.) MUST stay flat on each type.

### Rule 2: DoConnect lives on DataSession only

`ConnectOptions.DoConnect` is a stub that returns an error. The real `DoConnect` is on `DataSession`. Never add network-stack setup logic to `ConnectOptions`.

### Rule 3: Cleanup routes by type identity

`ConnectOptions.Cleanup` is ALWAYS `cleanupControlPlane`. `DataSession.Cleanup` is ALWAYS `cleanupDataPlane`. No nil-check or flag needed — the type IS the discriminant.

### Rule 4: Persistence only happens in the User Daemon

`OffloadToConfig` type-asserts to `*handler.ConnectOptions`. `DataSession` is never persisted. Fields that need to survive daemon restarts must be on `ConnectOptions` with a `json:` tag.

### Rule 5: Do not assume the two daemons' session instances share state

They communicate via gRPC and do not share memory. Any data that needs to cross daemon boundaries must be transmitted via `rpc.ConnectRequest`/`rpc.ConnectResponse`.

## 5. Common Mistake Examples

### Wrong: Adding a data-plane field to ConnectOptions

```go
type ConnectOptions struct {
    ...
    tunName string  // Wrong! User Daemon does not create TUN devices
}
```

### Correct: Add it to DataSession

```go
type DataSession struct {
    ...
    nm *NetworkManager  // Root Daemon only: full network stack
}
```

### Wrong: Calling DoConnect on a ConnectOptions (ControlSession)

```go
connect := &handler.ConnectOptions{...}
connect.DoConnect(ctx)  // Wrong! Returns error: "DoConnect is not available on a control-plane session"
```

### Correct: Use DataSession for the root daemon

```go
ds := &handler.DataSession{...}
ds.DoConnect(ctx)  // Root Daemon: start full network stack
```
