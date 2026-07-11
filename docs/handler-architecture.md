# Handler Package Architecture

## Overview

`pkg/handler` contains the core business logic for VPN connections, proxy workload management, and file synchronization. After refactoring, responsibility is distributed across three layers:

```
ConnectOptions (session orchestrator)
├── NetworkManager (route/TUN/DNS)
├── ProxyManager (sidecar inject/leave)
└── K8sClient (shared Kubernetes access)
```

## ConnectOptions — Session Orchestrator

`ConnectOptions` is the top-level struct representing a VPN connection session. It coordinates initialization order, delegates networking and proxy concerns to sub-managers, and owns the session lifecycle (context, cleanup).

### Dual-Daemon Role

The same struct runs in both daemon layers with independent instances:

| | User Daemon (control plane) | Root Daemon (data plane) |
|---|---|---|
| Entry point | `redirectConnectToSudoDaemon` | `Connect` → `DoConnect` |
| `isDataPlane` | false | true |
| Role | DHCP, proxy inject, health check | TUN, port-forward, DNS, routes |
| Persisted | Yes (OffloadToConfig) | No |

### Key Fields

```go
type ConnectOptions struct {
    K8sClient                          // embedded: clientset, config, factory

    ManagerNamespace      string       // namespace of traffic manager infrastructure
    WorkloadNamespace     string       // namespace of user's target workloads
    OriginKubeconfigPath  string       // user's kubeconfig file path

    network      *NetworkManager       // route/TUN delegation (data plane only)
    proxyManager *ProxyManager         // proxy workload delegation (control plane only)
    dhcp         *dhcp.Manager         // DHCP IP allocation

    LocalTunIPv4 *net.IPNet            // leased TUN IPv4 address
    LocalTunIPv6 *net.IPNet            // leased TUN IPv6 address
    RequestRaw   []byte                // serialized ConnectRequest for restart replay
}
```

### Initialization Sequence (DoConnect — data plane)

```
1. InitDHCP           → allocate IPs from ConfigMap
2. getCIDR            → detect cluster Pod/Service CIDRs
3. createOutboundPod  → ensure CIDR detection pod exists
4. upgradeDeploy      → upgrade traffic manager if needed
5. AddExtraNodeIP     → add node IPs to extra CIDR list
6. portForward        → port-forward to traffic manager (gvisor TCP/UDP)
7. startLocalTunServer → create local TUN device + gvisor stack
8. NetworkManager.AddRouteDynamic → watch pods/services, add routes
9. setupDNS           → configure local DNS resolution
```

## NetworkManager — Route Table Management

Owns route-related state and operations. Created during `DoConnect` after TUN device is established.

### Responsibilities

- Add/remove routes on the TUN device
- Watch pod/service informers and keep route table updated
- Resolve extra domains via dig on the traffic manager pod
- Add cluster node IPs to the extra CIDR list

### Key Methods

| Method | Description |
|--------|-------------|
| `AddRoute(ips...)` | Add IPs to route table, skip API server IPs |
| `AddRouteDynamic(ctx)` | Start informers for dynamic routing |
| `AddExtraRoute(ctx, podName)` | Resolve extra domains, add to routes |
| `AddExtraNodeIP(ctx)` | Add all node IPs to extra CIDR list |

### Field Semantics

```go
type NetworkManager struct {
    clientset          kubernetes.Interface
    config             *rest.Config
    managerNamespace   string         // traffic manager namespace (for Shell/dig calls)
    workloadNamespace  string         // user workload namespace (for informer scope)

    tunName            string         // TUN device name (set after TUN creation)
    apiServerIPs       []net.IP       // IPs excluded from routing
    extraHost          []dns.Entry    // accumulated domain→IP mappings
    extraRoute         *ExtraRouteInfo // user-specified extra routes/domains
}
```

## ProxyManager — Proxy Workload Lifecycle

Owns the list of currently proxied workloads and handles leave/unpatch operations.

### Responsibilities

- Track which workloads are being intercepted (Add/Remove)
- Unpatch sidecar containers on leave (restore original pod spec)
- Remove envoy config from the shared ConfigMap
- Thread-safe access via mutex

### Key Methods

| Method | Description |
|--------|-------------|
| `Add(proxy)` | Register a newly proxied workload |
| `Remove(ns, workload)` | Remove a workload from tracking |
| `Resources()` | Snapshot of all tracked proxy workloads |
| `IsMe(ns, uid, headers)` | Check ownership of a proxy rule |
| `LeaveAll(ctx, localIPv4)` | Unpatch all tracked workloads |
| `Leave(ctx, resources, v4)` | Unpatch specific workloads |

### Field Semantics

```go
type ProxyManager struct {
    factory            cmdutil.Factory
    clientset          kubernetes.Interface
    managerNamespace   string         // for ConfigMap operations (envoy config)

    mu                 sync.Mutex
    workloads          ProxyList      // currently intercepted workloads
}
```

## K8sClient — Shared Kubernetes Access

Embedded struct providing Kubernetes client initialization and access.

```go
type K8sClient struct {
    clientset   kubernetes.Interface
    restclient  *rest.RESTClient
    config      *rest.Config
    factory     cmdutil.Factory
}
```

Methods: `InitClient(f)`, `GetFactory()`, `GetClientset()`.

Embedded by both `ConnectOptions` and `SyncOptions` to share the initialization pattern.

## Connection Interface

Defined in `connection.go`, documents the API surface that `daemon/action` uses:

```go
type Connection interface {
    // Lifecycle
    InitClient(f cmdutil.Factory) error
    DoConnect(ctx context.Context) error
    Cleanup(ctx context.Context)
    Context() context.Context
    AddRollbackFunc(f func() error)

    // Identity
    GetConnectionID() string
    GetLocalTunIP() (v4, v6 string)
    IsMe(ns, uid string, headers map[string]string) bool

    // Health
    HealthCheckOnce(ctx context.Context, timeout time.Duration)
    HealthPeriod(ctx context.Context, interval time.Duration)
    HealthStatus() HealthStatus

    // Proxy
    CreateRemoteInboundPod(ctx, ns, workloads, headers, portMap, image) error
    LeaveAllProxyResources(ctx) error
    ProxyResources() ProxyList

    // Accessors
    GetManagerNamespace() string
    GetOriginKubeconfigPath() string
    GetFactory() cmdutil.Factory
}
```

This interface enables future migration where `daemon/action` depends on the interface rather than the concrete `*ConnectOptions` type.

## Namespace Convention

All namespace fields use unambiguous full names:

| Field | Meaning | Example |
|-------|---------|---------|
| `ManagerNamespace` / `managerNamespace` | Where traffic manager runs | `"kubevpn"` |
| `WorkloadNamespace` / `workloadNamespace` | Where user's apps run | `"default"` |

The RPC layer uses `req.Namespace` (workload) and `req.ManagerNamespace` (manager). See `docs/namespace-model.md` for full details.

## Dependency Rules

```
pkg/daemon/action → pkg/handler (via Connection interface, eventually)
pkg/handler       → pkg/config, pkg/util, pkg/core, pkg/inject, pkg/dns, pkg/tun, pkg/dhcp
pkg/ssh           → pkg/config, pkg/util (NO daemon/rpc dependency)
pkg/util          → pkg/config (NO upward dependencies)
pkg/core          → pkg/config, pkg/util, pkg/tun
```

### Forbidden

- `pkg/util` must NOT import `pkg/daemon/rpc` or any higher-level package
- `pkg/ssh` must NOT import `pkg/daemon/rpc`
- `pkg/handler` must NOT import `pkg/daemon/rpc` in core types (conversion adapters are acceptable)

## File Layout

```
pkg/handler/
├── connect.go              ConnectOptions struct, DoConnect, DHCP, CIDR
├── connect_tun.go          portForward, startLocalTunServer
├── connect_route.go        newTickerResetHandler (utility)
├── connect_dns.go          setupDNS
├── connect_upgrade.go      upgradeDeploy
├── network.go              NetworkManager struct + route methods
├── proxy_manager.go        ProxyManager struct + leave methods
├── proxy.go                Proxy/ProxyList data types
├── proxy_mapper.go         Mapper for port-forward config watching
├── k8s_client.go           K8sClient embedded struct
├── connection.go           Connection interface definition
├── healthchecker.go        HealthStatus, HealthPeriod
├── cleaner.go              Cleanup, signal handler
├── sync.go                 SyncOptions, DoSync
├── sync_containers.go      Sync container generation
├── traffmgr.go             Traffic manager pod creation
├── traffmgr_resources.go   K8s resource generators (deploy, svc, etc.)
├── leave.go                Delegating leave methods
├── reset.go                Reset workloads to original spec
├── once.go                 ConfigMap informer (singleton)
├── sort.go                 Connects sorting
├── extraoptions.go         ExtraRouteInfo type + RPC conversion
└── testing.go              Test helpers
```
