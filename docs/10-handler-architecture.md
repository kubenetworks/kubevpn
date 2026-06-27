# Handler Package Architecture

## Overview

`pkg/handler` is the core business logic layer for VPN connections, proxy management, and file synchronization. Responsibilities are distributed across four sub-managers:

```
ConnectOptions (session orchestrator)
├── NetworkManager      — full networking lifecycle (port-forward, TUN, routes, DNS) + IP hot-reload
├── ProxyManager        — sidecar injection/removal
├── ConfigMapStore      — ConfigMap read/write + health checks
└── K8sClient           — Kubernetes access (embedded)
```

## ConnectOptions — Session Orchestrator

```go
type ConnectOptions struct {
    K8sClient                       // embedded: clientset, config, factory

    // Configuration (set during daemon layer creation)
    ManagerNamespace     string     // namespace where traffic manager resides
    WorkloadNamespace    string     // user workload namespace
    ExtraRouteInfo       ExtraRouteInfo
    Image                string
    OriginKubeconfigPath string
    OwnerID              string     // UUID, uniquely identifies this connection (used by TunConfigService)
    RequestRaw           []byte     // serialized ConnectRequest (used for daemon restart replay)

    // Session lifecycle
    ctx, cancel          context.Context, context.CancelFunc
    rollbackFuncList     []func() error

    // Sub-managers
    network              *NetworkManager
    proxyManager         *ProxyManager
    configMapStore       *ConfigMapStore
}
```

### Dual-Daemon Roles

| | User Daemon (control plane) | Root Daemon (data plane) |
|---|---|---|
| Entry point | `redirectConnectToSudoDaemon` | `Connect` → `DoConnect` |
| IP acquisition | queries sudo daemon via `getSudoTunIPs` | `NetworkManager.rentIP` (in startTUN) |
| Sub-managers | proxyManager, configMapStore | network, configMapStore |

### IP Allocation Flow

```
User Daemon:
  CreateOutboundPod → UpgradeDeploy
  → req.OwnerID = connect.OwnerID
  → cli.Connect(ctx) → forwards to Root Daemon

Root Daemon:
  connect.OwnerID = req.OwnerID
  DoConnect → NetworkManager.Start()
    → startTUN → rentIP(OwnerID, ExcludeIPs)
      → gRPC GetTunIP(ownerID) → TunConfigServer DHCP rent
      → 198.18.0.5/32
    → create TUN device
  → StartIPWatcher(ctx)
    → WatchTunIP(ownerID) stream
    → IP change → ChangeTunIP()

User Daemon (when IP is needed):
  ips := svr.getSudoTunIPs(ctx)         // query sudo daemon Status
  v4, v6 := resolveTunIP(connect, ips)  // match by ConnectionID
```

**IP is entirely managed by the Root Daemon's NetworkManager.** The User Daemon no longer holds `LocalTunIPv4/v6`.

## NetworkManager — Full Networking Lifecycle

```go
type NetworkManager struct {
    cfg       NetworkConfig    // immutable configuration
    ctx       context.Context  // independent context (Stop only cancels networking)
    cancel    context.CancelFunc
    tunName   string
    extraHost []dns.Entry
    dnsConfig *dns.Config
}

type NetworkConfig struct {
    Clientset, RESTClient, Config  // K8s access
    ManagerNamespace, WorkloadNamespace
    CIDRs, APIServerIPs            // routing parameters
    ExtraRouteInfo                 // extra routes/domains
    Image, Lock                    // auxiliary
    OwnerID                        // used by IP watcher
    GetRunningPodList              // dependency injection
}
```

### Lifecycle API

| Method | Description |
|--------|-------------|
| `Start(ctx)` | Starts: port-forward → TUN → routes → DNS |
| `Stop()` | Stops: DNS → cancel context |
| `ChangeTunIP(ctx, v4, v6)` | Hot-reload TUN IP (without rebuilding the network stack) |
| `StartIPWatcher(ctx)` | Background listener for TunConfigService IP change pushes |
| `TunName()` | Returns the TUN device name |

### IP Hot-Reload Flow

```
TunConfigService push (WatchTunIP stream)
  → NetworkManager.ChangeTunIP(newIPv4, newIPv6)
    → tun.ChangeIP(tunName, old, new)     // OS-level change (fd unchanged)
    → update nm.localTunIPv4/v6
    → heartbeat automatically uses new IP on next tick   // re-read from OS
    → Server RouteHub automatically registers new IP     // AddRoute per packet
```

## TunConfigService — IP Configuration Hub

Runs on the Traffic Manager Pod's control-plane gRPC server (port 9002):

```protobuf
service TunConfigService {
  rpc GetTunIP(TunIPRequest) returns (TunIPResponse);      // allocate/renew
  rpc WatchTunIP(TunIPRequest) returns (stream TunIPResponse); // change push
  // No ReleaseTunIP — follows DHCP protocol, leases expire and are reclaimed automatically
}

message TunIPRequest {
  string OwnerID = 1;
  string Namespace = 2;
  repeated string ExcludeIPs = 4;  // client local interface IPs, skipped during allocation
}
```

### Conflict Avoidance — ExcludeIPs

- `rentIP` passes all local interface IPs as `ExcludeIPs` to the server
- The server calls `dhcp.RentIPExcluding(excludeIPs)`, skipping conflicting IPs in a single DHCP transaction
- Typically succeeds in one call; a lightweight retry (up to 15 attempts) handles non-atomic race conditions

### Lease Renewal Mechanism

- Each `GetTunIP` call refreshes the `LastRenew` timestamp (doubles as lease renewal)
- While a `WatchTunIP` stream is active, the server automatically refreshes `LastRenew` every `LeaseDuration/3` (~100s) (implicit renewal)
- `StartLeaseReaper` runs in the background, scanning for expired allocations every 30s (TTL = 5 min)
- Expired IPs are automatically reclaimed to the DHCP pool
- **Explicit Release is no longer needed** — after a crash, leases expire and are reclaimed automatically
- After a WatchTunIP stream disconnects, if the client no longer calls GetTunIP, the IP is reclaimed after 5 minutes

### OwnerID

| Scenario | OwnerID Value | Lifecycle |
|----------|---------------|-----------|
| Client (connect/proxy) | `uuid.New().String()[:12]` | Unique per connection |
| Sidecar (mesh mode) | `podName` | Pod lifecycle |

**Why not connectionID:** connectionID = last 12 hex chars of the namespace UID, which is per-namespace. Multiple clients connecting to the same namespace would conflict.

## ProxyManager — Sidecar Injection/Removal

```go
type ProxyManager struct {
    factory            cmdutil.Factory
    clientset          kubernetes.Interface
    managerNamespace   string
    mu                 sync.Mutex
    workloads          ProxyList
}
```

| Method | Description |
|--------|-------------|
| `Add(proxy)` | Register a proxy workload |
| `Remove(ns, workload)` | Remove from tracking |
| `Resources()` | Snapshot the list |
| `LeaveAll(ctx, ownerID)` | Remove all sidecars for the given ownerID |
| `Leave(ctx, resources, ownerID)` | Remove sidecars from specified workloads for the given ownerID |

## ConfigMapStore — ConfigMap + Health Checks

```go
type ConfigMapStore struct {
    clientset, managerNamespace
    informerOnce, informer, informerStop
    healthStatus HealthStatus
}
```

**Lazy initialization:** Created on first access via `getConfigMapStore()`, ensuring `ManagerNamespace` has already been corrected by `detectAndSetManagerNamespace`.

## Namespace Conventions

| Field | Meaning |
|-------|---------|
| `ManagerNamespace` / `managerNamespace` | Namespace where the traffic manager resides |
| `WorkloadNamespace` / `workloadNamespace` | User workload namespace |
| RPC `req.Namespace` | Always the workload namespace |
| RPC `req.ManagerNamespace` | Always the manager namespace |

## Dependency Rules

```
pkg/daemon/action  → pkg/handler (via Connection interface)
pkg/daemon/grpcutil → pkg/daemon/rpc
pkg/handler        → pkg/config, pkg/util, pkg/core, pkg/inject, pkg/dns, pkg/tun
pkg/handler        → pkg/daemon/rpc (gRPC client only: TunConfigService)
pkg/ssh            → pkg/config, pkg/util (no dependency on daemon/rpc)
pkg/util           → pkg/config (no dependency on upper-layer packages)
pkg/controlplane   → pkg/dhcp, pkg/daemon/rpc (server-side DHCP + TunConfigService)
```

**Client side no longer depends on pkg/dhcp** — IP allocation is entirely managed by the server-side TunConfigService.

## File Layout

```
pkg/handler/
├── connect.go            ConnectOptions, DoConnect, getCIDR
├── connect_tun.go        Run() server runner, healthCheck helpers
├── connect_dns.go        detectNameserver helpers
├── connect_route.go      newTickerResetHandler
├── connect_upgrade.go    upgradeDeploy
├── network.go            NetworkManager (Start/Stop/ChangeTunIP/IPWatcher)
├── proxy_manager.go      ProxyManager (Add/Remove/Leave)
├── configmap_store.go    ConfigMapStore (Get/Set/Health)
├── proxy.go              Proxy/ProxyList data types
├── proxy_mapper.go       Mapper (port-forward config watcher)
├── k8s_client.go         K8sClient embedded struct
├── connection.go         Connection interface definition
├── healthchecker.go      HealthStatus type
├── cleaner.go            Cleanup, signal handling
├── sync.go               SyncOptions, DoSync
├── traffmgr.go           Traffic manager pod creation
├── traffmgr_resources.go K8s resource generators
├── leave.go              Proxy removal delegation
├── reset.go              Reset workloads
├── once.go               Server-side helpers (labelNs, genTLS, getCIDR)
├── sort.go               Connects sorting
├── extraoptions.go       ExtraRouteInfo + RPC conversion
├── sshconv.go            SSH config ↔ RPC conversion
└── testing.go            Test helpers
```
