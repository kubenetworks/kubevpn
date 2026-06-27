# Dual Daemon Architecture

## Why This Document Exists

KubeVPN uses a dual-process architecture (User Daemon + Root Daemon), where each process holds an **independent `ConnectOptions` instance** with completely different field semantics and usage scenarios. In the past, the lack of this architecture document led to bugs where fields were initialized in the wrong daemon (e.g., OwnerID being generated uselessly in the Root Daemon).

**Anyone modifying `ConnectOptions` fields or `daemon/action/` code must read this document first.**

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
│  ├── SSH jump host (resolveKubeconfig)                │
│  ├── Traffic Manager creation/upgrade (CreateOutboundPod/UpgradeDeploy)│
│  ├── Proxy injection (CreateRemoteInboundPod → inject/)│
│  ├── File sync (DoSync)                               │
│  ├── Health checks (HealthPeriod)                     │
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
│  Holds: its own ConnectOptions (data plane)            │
│  Storage: svr.connections slice                        │
└─────────────────────────────────────────────────────┘
```

## 2. ConnectOptions Dual Instances

**Key rule: The User Daemon and Root Daemon each create independent `ConnectOptions` instances. They do NOT share memory.**

### 2.1 User Daemon's ConnectOptions (Control Plane)

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
| `K8sClient` (clientset, factory) | Interact with K8s API (inject sidecar, query ConfigMap) |
| `OwnerID` | Written into Envoy Rule to identify ownership |
| `Image/ImagePullSecretName` | CreateOutboundPod creates the traffic manager pod |
| `proxyWorkloads` | Track currently proxied workloads |
| `healthStatus` | Periodic health checks |
| `Sync` | File sync options |
| `RequestRaw` | Needed for persistence |

**Unused fields (always zero-valued in the User Daemon)**:
| Field | Reason |
|-------|--------|
| `ctx/cancel` | DoConnect is not called, so these are never set |
| `isDataPlane` | Always false |
| `tunName` | Does not create TUN devices |
| `dnsConfig` | Does not configure DNS |
| `cidrs` | Does not detect CIDRs (Root Daemon does) |
| `apiServerIPs` | Does not perform route filtering |

### 2.2 Root Daemon's ConnectOptions (Data Plane)

Creation site: `daemon/action/connect.go` → `Connect()` (IsSudo=true branch)

```go
connect := &handler.ConnectOptions{
    ManagerNamespace:     req.ManagerNamespace,
    ExtraRouteInfo:       ...,
    OriginKubeconfigPath: req.OriginKubeconfigPath,
    WorkloadNamespace:    req.Namespace,
    Lock:                 &svr.Lock,
    Image:                req.Image,
    ImagePullSecretName:  req.ImagePullSecretName,
    RequestRaw:           reqBytes,
    // Note: no OwnerID — Root Daemon does not operate on Envoy config
    // Note: does not call CreateOutboundPod/UpgradeDeploy — that is the control plane's responsibility
}
```

Fields used:
| Field | Purpose |
|-------|---------|
| `ctx/cancel` | Created by DoConnect, controls data plane lifecycle |
| `isDataPlane` | Set to true by DoConnect |
| `network` | NetworkManager: TUN, port forwarding, routes, DNS |
| `configMapStore` | ConfigMap informer for CIDR caching |

**Unused fields (meaningless in the Root Daemon)**:
| Field | Reason |
|-------|--------|
| `OwnerID` | Does not operate on Envoy config |
| `proxyWorkloads` | Does not manage proxy workloads |
| `healthStatus` | Does not perform Envoy health checks (User Daemon does) |
| `Sync` | File sync runs in the User Daemon |

## 3. Operation Flows

### 3.1 Connect Flow

```
CLI: kubevpn connect
  │
  ▼
User Daemon: Connect RPC
  ├── redirectConnectToSudoDaemon()
  │     ├── Create ConnectOptions (control plane, with OwnerID, Image)
  │     ├── resolveKubeconfig (SSH jump host)
  │     ├── InitClient
  │     ├── detectAndSetManagerNamespace
  │     ├── forwardConnectToSudo()
  │     │     ├── CreateOutboundPod (create traffic manager pod)
  │     │     ├── UpgradeDeploy (upgrade traffic manager)
  │     │     ├── Set req.OwnerID, forward req to Root Daemon ──┐
  │     │     ├── Wait for Root Daemon to complete               │
  │     │     ├── Start HealthPeriod                             │
  │     │     └── Store in svr.connections                       │
  │                                                              ▼
  │                                    Root Daemon: Connect RPC
  │                                      ├── Create ConnectOptions (data plane)
  │                                      ├── connect.OwnerID = req.OwnerID
  │                                      ├── DoConnect()
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
        ├── ReleaseIP (return DHCP lease)
        ├── Delete temporary Pod/Job
        ├── LeaveAllProxyResources (clean up Envoy Rules, matched by OwnerID)
        └── rollback functions
```

## 4. Rules for Modifying ConnectOptions

### Rule 1: Determine which daemon a new field belongs to before adding it

Ask yourself: is this field used by `DoConnect` (data plane), or by `CreateRemoteInboundPod`/`LeaveResource`/`HealthPeriod` (control plane)?

- **Data plane**: does not need a json tag, does not need persistence, does not need to be set in `redirectConnectToSudoDaemon`
- **Control plane**: needs a json tag (for persistence), needs to be initialized in `redirectConnectToSudoDaemon`

### Rule 2: Do not initialize control plane fields in DoConnect

`DoConnect` only runs in the Root Daemon. Any field used only by the User Daemon (e.g., OwnerID) initialized in DoConnect is **dead code** — the Root Daemon's ConnectOptions and the User Daemon's are different objects.

### Rule 3: Persistence only happens in the User Daemon

`OffloadToConfig` is called in the `!svr.IsSudo` path. Only the User Daemon's ConnectOptions is serialized. Fields that need to survive restarts must have a `json:` tag.

### Rule 4: Do not assume the two daemons' ConnectOptions share state

They communicate via gRPC and do not share memory. Any data that needs to cross daemon boundaries must be transmitted via `rpc.ConnectRequest`/`rpc.ConnectResponse`.

## 5. Common Mistake Examples

### Wrong: Initializing a control-plane-only field in DoConnect

```go
func (c *ConnectOptions) DoConnect(ctx context.Context) (err error) {
    c.OwnerID = uuid.New().String()[:12]  // Useless! Root Daemon does not use OwnerID
    ...
}
```

### Correct: Initialize it when constructing ConnectOptions in the User Daemon

```go
// daemon/action/connect.go → redirectConnectToSudoDaemon()
connect := &handler.ConnectOptions{
    ...
    OwnerID: uuid.New().String()[:12],  // User Daemon uses OwnerID
}
```

### Wrong: Setting data plane fields in redirectConnectToSudoDaemon

```go
connect := &handler.ConnectOptions{
    ...
    tunName: "utun0",  // Useless! User Daemon does not create TUN devices
}
```

### Correct: Let DoConnect set data plane fields

```go
func (c *ConnectOptions) DoConnect(ctx context.Context) (err error) {
    ...
    c.network.Start(ctx)  // Root Daemon: start full network stack (port-forward, TUN, routes, DNS)
}
```
