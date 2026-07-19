# TUN IP Hot Update Design

This document describes how TUN IPs are allocated and kept consistent across the **client side** (local machine) and the **sidecar side** (in-cluster workloads). Both sides obtain their IP from the same `TunConfigService` gRPC API provided by the control-plane.

The two sides differ in how they handle a *runtime* IP change:

- **Client side** — the client's TUN device IP can change at runtime (e.g. DHCP lease expiry after a sleep/wake cycle). The client subscribes to `WatchTunIP` and hot-updates its own TUN device via `NetworkManager.ChangeTunIP`.
- **Sidecar side** — the sidecar fetches its TUN IP **once at startup** and does not hot-update its own device. When a *client's* IP changes, the routing target is updated at the control-plane: `syncEnvoyRuleIP` rewrites `Rule.LocalTunIPv4/v6` in `ENVOY_CONFIG`, and envoy's xDS stream pushes the new endpoint to the sidecar. There is **no sidecar-side TUN watcher and no iptables DNAT to a hardcoded client IP** — that mechanism was removed by the unified proxy mode.

> The unified proxy mode (VPN-only and Mesh both routed through envoy, TCP + UDP) and the IP-change push fix are specified in **[28-sleep-wake-ip-update.md](28-sleep-wake-ip-update.md)** — read it for the authoritative end-to-end design. This document focuses on the shared `TunConfigService` API and the client-side hot-update path.

---

## Sidecar Side

### 1. Problem Background

Earlier designs allocated sidecar TUN IPs once by a MutatingWebhook at Pod creation time, with no way to change them afterward. The webhook has since been removed; sidecars now self-allocate from the control-plane at startup (see below).

### 2. Design Goals

- **Leverage the existing envoy control-plane infrastructure as a configuration hub** for distributing TUN IPs
- Sidecar requests an IP from the control-plane at startup (passing its OwnerID)
- Control-plane pushes new configuration to the sidecar when an IP change is detected
- Sidecar falls back to periodic polling as a safety net
- Remove IP allocation responsibility from the webhook

### 3. Core Idea

The existing envoy control-plane (`pkg/controlplane`) already provides:
- A gRPC server (xDS protocol, port 9002)
- A ConfigMap watcher that monitors `kubevpn-traffic-manager` ConfigMap changes
- A Processor that converts Virtual/Rule configurations from the ConfigMap into envoy snapshots

**Extension point:** Add a new **TUN IP configuration service** on the control-plane's gRPC server. Sidecars obtain and watch their TUN IP configuration over the same gRPC connection.

```
                   Traffic Manager Pod
                   +--------------------------------+
                   |  envoy control-plane (gRPC:9002)|
                   |  +-- xDS (envoy config)         |
                   |  +-- TunConfig service (NEW)    |
                   |      +-- GetTunIP(ownerID)      |
                   |      +-- WatchTunIP(ownerID)    |
                   |                                |
                   |  ConfigMap watcher              |
                   |  +-- KeyEnvoy     -> envoy snap  |
                   |  +-- KeyTunIPPool -> TUN IP alloc|
                   +--------------------------------+
                          ^ gRPC
                          |
              +-----------+-----------+
              |  Sidecar (workload)    |
              |  kubevpn server        |
              |  +-- startup: GetTunIP |
              |      (once, no watcher) |
              |  envoy sidecar         |
              |  +-- xDS endpoint push |
              |      -> route to new   |
              |      client TUN IP     |
              +------------------------+
```

### 4. Component Design

#### 4.1 New gRPC Service Definition

Register a new service on the existing control-plane gRPC server (no new port required):

```protobuf
// pkg/daemon/rpc/daemon.proto (additions)

service TunConfigService {
  // GetTunIP allocates or retrieves the current TUN IP.
  // ownerID identifies the caller (sidecar uses podName or connectionID).
  // If the ownerID already has an allocation, the existing IP is returned;
  // otherwise a new IP is allocated via DHCP.
  rpc GetTunIP(TunIPRequest) returns (TunIPResponse);
  
  // WatchTunIP is a long-lived stream that pushes new configuration
  // whenever the IP for the given ownerID changes.
  rpc WatchTunIP(TunIPRequest) returns (stream TunIPResponse);
}

message TunIPRequest {
  string OwnerID = 1;              // sidecar identity (podName or nodeID)
  string Namespace = 2;            // workload namespace
  repeated string ExcludeIPs = 4;  // client local interface IPs to skip during allocation
}

message TunIPResponse {
  string IPv4 = 1;           // e.g. "198.18.0.5/32"
  string IPv6 = 2;           // e.g. "fd11::5/128"
  int64  Version = 3;        // configuration version for change detection
}
```

#### 4.2 Control-Plane Implementation

Add TUN IP management logic in `pkg/controlplane/`:

```go
// pkg/controlplane/tun_config.go (new file)

type TunConfigServer struct {
    rpc.UnimplementedTunConfigServiceServer
    
    dhcp      *dhcp.Manager
    mu        sync.RWMutex
    allocated map[string]*allocation  // ownerID -> IP allocation
    watchers  map[string][]chan *rpc.TunIPResponse
}

type allocation struct {
    IPv4    *net.IPNet
    IPv6    *net.IPNet
    Version int64
}

// GetTunIP: if ownerID already has an allocation and it does not conflict,
// return the existing IP. If the allocation is in ExcludeIPs, reallocate.
// Otherwise allocate a new IP.
// ExcludeIPs contains client local interface IPs; the server skips them during DHCP allocation.
func (s *TunConfigServer) GetTunIP(ctx context.Context, req *rpc.TunIPRequest) (*rpc.TunIPResponse, error) {
    excludeIPs := parseExcludeIPs(req.ExcludeIPs)

    if alloc, ok := s.allocated[req.OwnerID]; ok {
        if !isIPExcluded(alloc.IPv4, excludeIPs) {
            return &rpc.TunIPResponse{...}, nil  // return as-is
        }
        // Conflict: old IP stays in the bitmap; RentIPExcluding naturally skips it
        delete(s.allocated, req.OwnerID)
        v4, v6 := s.dhcp.RentIPExcluding(ctx, excludeIPs)
        s.dhcp.ReleaseIP(ctx, old.IPv4.IP, old.IPv6.IP)  // release old IP
        s.allocated[req.OwnerID] = &allocation{IPv4: v4, IPv6: v6, ...}
        return &rpc.TunIPResponse{...}, nil
    }

    // New allocation
    v4, v6 := s.dhcp.RentIPExcluding(ctx, excludeIPs)
    s.allocated[req.OwnerID] = &allocation{IPv4: v4, IPv6: v6, ...}
    return &rpc.TunIPResponse{...}, nil
}

// WatchTunIP: long-lived stream that pushes on IP change
func (s *TunConfigServer) WatchTunIP(req *rpc.TunIPRequest, stream rpc.TunConfigService_WatchTunIPServer) error {
    ch := make(chan *rpc.TunIPResponse, 1)
    s.addWatcher(req.OwnerID, ch)
    defer s.removeWatcher(req.OwnerID, ch)
    
    for {
        select {
        case resp := <-ch:
            if err := stream.Send(resp); err != nil {
                return err
            }
        case <-stream.Context().Done():
            return nil
        }
    }
}

// NotifyIPChange: called when the ConfigMap watcher detects a DHCP data change
func (s *TunConfigServer) NotifyIPChange(ownerID string, newIPv4, newIPv6 *net.IPNet) {
    s.mu.Lock()
    alloc := s.allocated[ownerID]
    if alloc != nil {
        alloc.IPv4 = newIPv4
        alloc.IPv6 = newIPv6
        alloc.Version = time.Now().UnixNano()
    }
    s.mu.Unlock()
    
    // Push to all watchers
    s.notifyWatchers(ownerID, &rpc.TunIPResponse{
        IPv4: newIPv4.String(), IPv6: newIPv6.String(), Version: alloc.Version,
    })
}
```

#### 4.3 Registering the New Service on the Control-Plane gRPC Server

Modify `pkg/controlplane/server.go`:
```go
func runServer(ctx context.Context, server serverv3.Server, tunConfig *TunConfigServer, port uint) error {
    // ... existing setup ...
    
    // Register envoy xDS services (existing)
    discoverygrpc.RegisterAggregatedDiscoveryServiceServer(grpcServer, server)
    // ...
    
    // Register TUN IP configuration service (new)
    rpc.RegisterTunConfigServiceServer(grpcServer, tunConfig)
    
    return grpcServer.Serve(listener)
}
```

#### 4.4 Sidecar: Fetch IP at Startup + Background Watch

Modify the TUN protocol factory (`tunProtocolFactory`) in `kubevpn server`:

```go
func tunProtocolFactory(node *Node, hub *RouteHub) (net.Listener, Handler, error) {
    netAddr := node.Get("net")
    
    if netAddr == "" {
        // New path: obtain IP from the control-plane
        ownerID := os.Getenv(config.EnvPodName) // pod name as ownerID
        trafficManagerAddr := os.Getenv("TrafficManagerService")
        
        // gRPC connection to traffic manager port 9002
        conn, _ := grpc.Dial(trafficManagerAddr+":9002", grpc.WithInsecure())
        client := rpc.NewTunConfigServiceClient(conn)
        
        // Fetch IP
        resp, err := client.GetTunIP(ctx, &rpc.TunIPRequest{
            OwnerID:   ownerID,
            Namespace: os.Getenv(config.EnvPodNamespace),
        })
        if err != nil {
            return nil, nil, fmt.Errorf("get TUN IP from control-plane: %w", err)
        }
        netAddr = resp.IPv4
        net6Addr = resp.IPv6
        // No background watcher: the sidecar's own TUN IP is fixed for the
        // process lifetime. Runtime routing changes are handled by envoy xDS.
    }
    
    // Create TUN using the obtained IP
    listener, err := tun.Listener(tun.Config{Addr: netAddr, ...})
    ...
}
```

The actual implementation is `tunProtocolFactory` + `requestTunIPFromControlPlane` in `pkg/core/protocol_registry.go`. The factory fetches the IP once (when `net=` is empty) and creates the TUN device; it does **not** start any watcher goroutine.

#### 4.5 No Sidecar-Side Hot-Update (Removed)

Earlier designs ran a `watchTunIPChanges` / `pollTunIP` / `applyTunIPChange` loop inside the sidecar that hot-updated the sidecar's own TUN device and rewrote iptables DNAT rules. **All of this was removed** by the unified proxy mode ([28-sleep-wake-ip-update.md](28-sleep-wake-ip-update.md)).

The reasoning:

- The sidecar's own TUN IP only matters for its data-plane connection back to the traffic manager (`:10801`); it does not need to track client IP changes.
- The value that *does* change at runtime is the **client's** TUN IP, which is the envoy routing target (`Rule.LocalTunIPv4/v6`). That is now updated at the control-plane by `syncEnvoyRuleIP` and propagated to the sidecar's envoy via the standard `ConfigMap → Watcher → Processor → xDS` push. There is no hardcoded DNAT-to-client-IP anymore — all traffic is DNAT'd to the local envoy listener (`:15006`), and envoy routes to the current client IP.

So the sidecar's `kubevpn server` container has no IP-watch logic. `pkg/tun.ChangeIP` is now used **only on the client side** (see below); `pkg/tun.UpdateDNAT` is no longer called by either side (it remains as a low-level helper but has no callers after the unified proxy mode).

#### 4.6 Webhook IP Allocation (Removed)

The webhook (`pkg/webhook/`) has been fully removed from the codebase. IP allocation is now entirely handled by the TunConfigService gRPC API. Leases expire and are automatically reclaimed; no explicit release is needed.

### 5. ConfigMap Watcher Integration

The existing control-plane watcher already monitors ConfigMap changes. Extend it to detect DHCP field changes:

```go
// pkg/controlplane/watcher.go -- modify the notifyCh send logic

// Previously only sends KeyEnvoy changes:
notifyCh <- NotifyMessage{Content: configMap.Data[config.KeyEnvoy]}

// New: detect TUN_IP_POOL changes and notify TunConfigServer
if tunIPPoolChanged(old, new) {
    tunConfigServer.ReconcileDHCP(ctx)
}
```

`ReconcileDHCP` walks the current `TUN_IP_POOL` bitmap (via `dhcp.Manager.ForEach`), checks whether the IP for each registered ownerID is still allocated, and re-rents + pushes (`NotifyIPChange`) if not. The IPv4/IPv6 bitmaps and cluster CIDRs now live in the merged `TUN_IP_POOL` and `CLUSTER_CIDRS` keys respectively — see [03-dhcp-ip-allocation.md](03-dhcp-ip-allocation.md).

### 6. Full Lifecycle

```
Pod created (with sidecar, no webhook IP allocation)
  |
Sidecar container starts (kubevpn server -l "tun:/tcp://tm:10801?net=&route=...")
  -> gRPC dial traffic-manager:9002
  -> GetTunIP(ownerID=podName) -> 198.18.0.5/32   (once, at startup)
  -> createTun("utun0", "198.18.0.5/32")
  -> connect traffic-manager:10801 (data-plane TCP)
  -> heartbeat(198.18.0.5)
  (envoy sidecar starts in parallel: ADS stream to xDS :9002)
  |
  +-- Client IP change scenario --------------+
  |   A client (NOT this sidecar) gets a new   |
  |   TUN IP (e.g. sleep/wake lease expiry):   |
  |   -> control-plane GetTunIP allocates new  |
  |      IP -> syncEnvoyRuleIP rewrites        |
  |      Rule.LocalTunIPv4 in ENVOY_CONFIG     |
  |   -> Watcher -> Processor -> xDS push      |
  |   -> this sidecar's envoy routes to the    |
  |      new client IP (TCP + UDP)             |
  |   (the sidecar's own TUN device is         |
  |    untouched — no ChangeIP, no DNAT edit)  |
  |                                            |
  +-- Pod deleted ----------------------------+
      -> graceful shutdown
      -> WatchTunIP / data-plane stream closes
      -> after LeaseDuration without renewal,
         the sidecar's IP is reaped back to pool
```

### 7. Comparison

| | Webhook Mode (legacy) | Sidecar Self-Watch (superseded) | Control-Plane + envoy xDS (current) |
|---|---|---|---|
| IP allocation timing | Pod creation | Sidecar startup | Sidecar startup |
| Sidecar own-IP change at runtime | Requires Pod restart | Sidecar watches + `ChangeIP` + DNAT edit | Not needed (IP fixed for process life) |
| Client-IP routing change | Requires Pod restart | iptables DNAT to hardcoded client IP | `syncEnvoyRuleIP` → xDS endpoint push |
| K8s RBAC requirements | Webhook needs elevated privileges | Sidecar gRPC only | Sidecar gRPC only |
| Latency | Immediate (at Pod creation) | gRPC stream + OS reconfig | gRPC stream real-time push |
| Protocols | TCP only | TCP only | TCP + UDP (envoy udp_proxy) |

### 8. File Change List

| File | Operation | Description |
|------|-----------|-------------|
| `pkg/daemon/rpc/daemon.proto` | Modify | TunConfigService (GetTunIP / WatchTunIP) |
| `pkg/controlplane/tun_config.go` | Modify | TunConfigServer; `syncEnvoyRuleIP`; `WatchTunIP` ticker push |
| `pkg/controlplane/cache.go` | Modify | `Virtual.To()` emits UDP listener (`toUDPListener`) |
| `pkg/controlplane/server.go` | Modify | Register TunConfigService on the xDS gRPC server |
| `pkg/controlplane/watcher.go` | Modify | Notify TunConfigServer on `TUN_IP_POOL` changes |
| `pkg/tun/ip*.go` | — | `ChangeIP` (client side only) |
| `pkg/core/tun_client.go` | Modify | heartbeat dynamic IP reading (client side) |
| `pkg/core/protocol_registry.go` | Modify | `tunProtocolFactory` fetches IP once; watcher/poll/apply removed |
| `pkg/inject/container.go` | Modify | DNAT to envoy `:15006`; no hardcoded client-IP DNAT |
| `pkg/webhook/` | Removed | Entire webhook directory deleted; IP allocation via TunConfigService |

### 9. Compatibility

- `?net=` non-empty -> legacy path (use the IP from the env var directly)
- `?net=` empty -> current path (fetch IP from control-plane via GetTunIP)
- Unified proxy mode is **not** wire-compatible with old sidecars; after upgrading run `kubevpn reset` then re-`proxy` (see [28-sleep-wake-ip-update.md](28-sleep-wake-ip-update.md) §8)

### 10. Validation

1. `go build ./...`
2. `make gen` -- generate protobuf
3. `go test ./pkg/controlplane/... ./pkg/tun/... ./pkg/core/...`
4. Integration test:
   - Start traffic manager (with TunConfigService)
   - Start sidecar (with `net=` empty)
   - Verify sidecar obtains IP via gRPC and creates TUN
   - Call NotifyIPChange to simulate IP change
   - Verify sidecar receives the push and hot-updates the TUN
   - Disconnect the Watch stream; verify polling fallback works

### 11. Known Issue Fix

#### WatchTunIP Long-Lived Connection Lease Expiry (fixed)

In the original design, the `WatchTunIP` stream did not refresh `LastRenew`. After 5 minutes, the LeaseReaper would incorrectly reclaim the IP. This has been fixed by adding an implicit lease renewal ticker on the server side within `WatchTunIP` (refreshes every `LeaseDuration/3`).

---

## Client Side

### 1. Background

When a user runs `kubevpn connect` or `kubevpn proxy`, a TUN device is also created on the local (client) machine. Currently the IP is fixed after DHCP RentIP and cannot be changed for the lifetime of the connection.

Consistent with the sidecar side, the client side also needs TUN IP hot-update support, using the same control-plane TunConfigService mechanism.

### 2. Differences Between Client and Sidecar

| Dimension | Sidecar | Client (local) |
|-----------|---------|-----------------|
| Runtime location | Inside a Pod | User's local machine (root daemon) |
| Connection method | Direct to traffic-manager:9002 | port-forward -> traffic-manager:9002 |
| TUN management | protocol_registry.go | NetworkManager (pkg/handler/network.go) |
| DHCP | Via TunConfigService | Current: direct ConfigMap operation (user daemon) |
| Lifecycle | Pod lifecycle | Connection lifecycle (DoConnect -> Cleanup) |

**Key difference:** The client's TUN is managed by NetworkManager, which already has Start/Stop lifecycle methods. The sidecar's TUN is created by `tunProtocolFactory` in protocol_registry.

### 3. Design Goals

- Client side reuses the same `TunConfigService` gRPC API to obtain and watch IPs
- NetworkManager gains a `ChangeTunIP()` method for runtime modification
- Uses the same `tun.ChangeIP` low-level capability as the sidecar
- Heartbeat already supports dynamic IP reading (completed in prior work)
- When the control-plane pushes an IP change, NetworkManager applies it automatically

### 4. Architecture

```
User Machine
+---------------------------------------------------+
|  User Daemon (control plane)                       |
|  +-- ConnectOptions                                |
|  |   +-- OwnerID (passed to Root Daemon via req)   |
|  |   +-- HealthCheck                               |
|  |   +-- getSudoTunIPs -> query sudo daemon IP     |
|  |                                                 |
|  +-- -> gRPC -> Root Daemon                        |
|                                                    |
|  Root Daemon (data plane)                          |
|  +-- ConnectOptions.DoConnect()                    |
|  |   +-- NetworkManager.Start()                    |
|  |       +-- portForward (-> traffic-manager:10801)|
|  |       +-- startTUN (create TUN device)          |
|  |       +-- routes                                |
|  |       +-- DNS                                   |
|  |                                                 |
|  +-- New: TunIPWatcher (background)                |
|       +-- port-forward -> traffic-manager:9002     |
|       +-- WatchTunIP(ownerID=OwnerID(UUID))        |
|       +-- IP change -> NetworkManager.ChangeTunIP()|
+---------------------------------------------------+
         | port-forward
+---------------------------+
|  Traffic Manager Pod       |
|  TunConfigService:9002     |
|  +-- GetTunIP(ownerID)     |
|  +-- WatchTunIP(ownerID)   |
+---------------------------+
```

### 5. Component Design

#### 5.1 NetworkManager: New `ChangeTunIP` Method

```go
// pkg/handler/network.go

// ChangeTunIP hot-updates the TUN device IP without restarting the network stack.
// Updates: OS interface address, iptables (if applicable), internal state.
// The next heartbeat automatically uses the new IP.
func (nm *NetworkManager) ChangeTunIP(ctx context.Context, newIPv4, newIPv6 *net.IPNet) error {
    // 1. tun.ChangeIP on OS device
    if err := tun.ChangeIP(nm.tunName, nm.localTunIPv4.String(), newIPv4.String()); err != nil {
        return fmt.Errorf("change IPv4: %w", err)
    }
    if newIPv6 != nil && nm.localTunIPv6 != nil {
        _ = tun.ChangeIP(nm.tunName, nm.localTunIPv6.String(), newIPv6.String())
    }

    // 2. Update iptables (no-op on client -- client does not use DNAT)
    // tun.UpdateDNAT(nm.localTunIPv4.IP, newIPv4.IP)  // client doesn't need this

    // 3. Update internal state
    nm.localTunIPv4 = newIPv4
    nm.localTunIPv6 = newIPv6

    plog.G(ctx).Infof("[NetworkManager] TUN IP changed: v4=%s v6=%s", newIPv4, newIPv6)
    return nil
}
```

#### 5.2 `rentIP` Conflict Avoidance -- ExcludeIPs Approach

`NetworkManager.rentIP` passes all local interface IPs as `ExcludeIPs` to the server, which skips them during DHCP allocation. Typically a single call returns a conflict-free IP.

A lightweight retry is kept for non-atomic race conditions (a new interface may appear between collecting local IPs and allocation):

```go
const maxRetries = 15
for i := 0; i < maxRetries; i++ {
    resp, err := client.GetTunIP(ctx, &rpc.TunIPRequest{
        OwnerID:    nm.cfg.OwnerID,
        Namespace:  nm.cfg.ManagerNamespace,
        ExcludeIPs: collectLocalIPs(),  // re-collect each iteration
    })
    v4 := parse(resp.IPv4)
    if !isLocalIPConflict(v4.IP) {
        return nil  // success
    }
    // Extremely rare race: a new interface appeared between collection and allocation
}
```

**Server-side handling:**
- Existing allocation not in ExcludeIPs -> return as-is (lease renewal)
- Existing allocation in ExcludeIPs -> keep old IP in bitmap, call `dhcp.RentIPExcluding(excludeIPs)` for a new IP (naturally skips the old one), then release the old IP
- No existing allocation -> `dhcp.RentIPExcluding(excludeIPs)` for a new allocation

**Why not ForceNew (release then rent):** DHCP uses `contiguousScanStrategy`, scanning sequentially from offset 0. Releasing then renting always returns the same IP, causing an infinite retry loop. The ExcludeIPs approach skips conflicting IPs within a single DHCP transaction, avoiding this problem.

#### 5.3 TunIPWatcher for Client (Runs in Root Daemon)

After `DoConnect` completes in the root daemon, a background goroutine starts watching TunConfigService:

```go
// pkg/handler/network.go or pkg/handler/tun_watcher.go

// StartIPWatcher connects to the control-plane's TunConfigService via port-forward
// and watches for IP changes. When a change is detected, it calls ChangeTunIP.
func (nm *NetworkManager) StartIPWatcher(ctx context.Context, ownerID string) {
    go nm.watchTunIPFromControlPlane(ctx, ownerID)
}

func (nm *NetworkManager) watchTunIPFromControlPlane(ctx context.Context, ownerID string) {
    // traffic manager port 9002 is exposed locally via port-forward
    // or accessed directly via K8s service (if kubeconfig permits)
    target := fmt.Sprintf("127.0.0.1:%d", controlPlaneLocalPort)

    var currentVersion int64
    for ctx.Err() == nil {
        conn, err := grpc.DialContext(ctx, target, grpc.WithInsecure())
        if err != nil {
            time.Sleep(10 * time.Second)
            continue
        }
        client := rpc.NewTunConfigServiceClient(conn)

        stream, err := client.WatchTunIP(ctx, &rpc.TunIPRequest{
            OwnerID:   ownerID,
            Namespace: nm.cfg.ManagerNamespace,
        })
        if err != nil {
            conn.Close()
            time.Sleep(30 * time.Second)
            continue
        }

        for {
            resp, err := stream.Recv()
            if err != nil {
                break
            }
            if resp.Version != currentVersion && currentVersion != 0 {
                newV4, newV6 := parseIPResponse(resp)
                nm.ChangeTunIP(ctx, newV4, newV6)
            }
            currentVersion = resp.Version
        }
        conn.Close()
        time.Sleep(5 * time.Second)
    }
}
```

#### 5.4 OwnerID Selection

Sidecar uses `podName` as ownerID. Client uses **OwnerID** (a UUID generated per connection):

```go
ownerID := c.OwnerID // uuid.New().String()[:12], created in connect_elevate.go
```

**Why not connectionID:**

- connectionID = last 12 hex chars of the namespace UID -- a **per-namespace** identifier
- Two users connecting to the same namespace share the same connectionID, so TunConfigService cannot distinguish them
- connectionID does not change on disconnect/reconnect, making it impossible to tell old sessions from new ones

**Advantages of OwnerID:**

- A new UUID is generated per connect call (globally unique)
- Available in the user daemon at creation time (before DHCP and TUN)
- Already used for envoy rule ownership tracking (`controlplane.Rule.OwnerID`)
- Persisted in daemon config (`json:"OwnerID"`); the same ID is reused on restart recovery
- Root Daemon receives this value via gRPC metadata (currently only IP is transmitted; OwnerID forwarding needs to be added)

#### 5.5 Port-Forward to Control-Plane 9002

Currently NetworkManager already port-forwards to the traffic manager's 10801/10802 ports (gvisor TCP/UDP). An additional port-forward for 9002 is needed for TunConfigService gRPC.

Two approaches:

**Approach A: Reuse the existing port-forward mechanism**
Add 9002 forwarding in NetworkManager.Start:

```go
portPair := []string{
    fmt.Sprintf("%d:10801", gvisorTCPPort),
    fmt.Sprintf("%d:10802", gvisorUDPPort),
    fmt.Sprintf("%d:9002", localControlPlanePort), // new
}
```

**Approach B: Independent port-forward**
A dedicated goroutine for port-forwarding 9002, with its lifecycle tied to the watcher.

**Approach A is recommended** -- simpler and reuses the existing mechanism.

#### 5.6 IP Acquisition Path in ConnectOptions

**Current flow:**

```
User Daemon: CreateOutboundPod -> UpgradeDeploy -> set req.OwnerID -> cli.Connect
Root Daemon: connect.OwnerID = req.OwnerID -> DoConnect -> NetworkManager.Start
  -> startTUN -> rentIP(OwnerID, ExcludeIPs) -> create TUN
  -> StartIPWatcher -> WatchTunIP -> ChangeTunIP on IP change
```

User Daemon no longer holds `LocalTunIPv4/v6`. When the IP is needed, it queries the sudo daemon's Status RPC via `getSudoTunIPs(ctx)` and matches by ConnectionID.

#### 5.7 User Daemon IP Query

User Daemon needs the TUN IP for sidecar injection, leave, and status display. It queries the sudo daemon via `getSudoTunIPs`:

```go
ips := svr.getSudoTunIPs(ctx)          // map[ConnectionID]tunIP
v4, v6 := resolveTunIP(connect, ips)   // match by ConnectionID
```

### 6. Full Lifecycle

```
kubevpn connect -n default
  |
  +-- User Daemon:
  |   1. Create ConnectOptions (with OwnerID = UUID[:12])
  |   2. InitClient -> configMapStore
  |   3. CreateOutboundPod -> UpgradeDeploy
  |   4. Set req.OwnerID, forwardConnectToSudo -> forward to Root Daemon
  |   5. When IP is needed: getSudoTunIPs -> resolveTunIP
  |
  +-- Root Daemon:
  |   1. connect.OwnerID = req.OwnerID
  |   2. DoConnect -> NetworkManager.Start()
  |      -> portForward(:10801, :10802, :9002)
  |      -> rentIP: GetTunIP(ownerID, ExcludeIPs=localIPs)
  |        -> server skips conflicting IPs -> 198.18.0.6/32
  |        -> isLocalIPConflict? -> no -> use this IP
  |      -> startTUN(198.18.0.6)
  |      -> routes, DNS
  |   3. StartIPWatcher(ownerID) (background)
  |      -> WatchTunIP via localhost:localPort -> 9002
  |
  +-- IP change scenario:
  |   External: control-plane decides to reallocate IP
  |   -> WatchTunIP stream push: 198.18.0.10/32
  |   -> Root Daemon: NetworkManager.ChangeTunIP()
  |      -> tun.ChangeIP("utun0", old, new)
  |      -> heartbeat automatically uses new IP
  |      -> Server RouteHub auto-register
  |   -> User Daemon: getSudoTunIPs returns new IP (for status/leave)
  |
  +-- Disconnect:
      -> Lease expires and is automatically reclaimed (no explicit release needed)
      -> Root Daemon: NetworkManager.Stop()
```

### 7. Comparison with Sidecar Implementation

| Step | Sidecar | Client |
|------|---------|--------|
| Obtain IP | `tunProtocolFactory` -> gRPC dial 9002 (once) | Root Daemon -> port-forward:9002 -> rentIP (in startTUN) |
| Create TUN | `tun.Listener` in protocol_registry | `NetworkManager.startTUN` |
| Watch for own-IP changes | None — IP fixed for process life | `NetworkManager.StartIPWatcher` (WatchTunIP) |
| Apply own-IP change | N/A | `NetworkManager.ChangeTunIP` (`tun.ChangeIP`) |
| Routing on client-IP change | envoy xDS endpoint push (`syncEnvoyRuleIP`) | N/A (the client *is* the routing target) |
| iptables | DNAT to envoy `:15006` (static) | Not needed (client does not use DNAT) |
| Release IP | Not needed -- lease expiry auto-reclaims | Not needed -- lease expiry auto-reclaims |
| ownerID | podName | OwnerID (UUID, unique per connection) |

### 8. Shared Code

| Component | Package | Shared by Sidecar and Client |
|-----------|---------|------------------------------|
| `tun.ChangeIP` | `pkg/tun` | Client only (sidecar IP is fixed) |
| heartbeat dynamic IP reading | `pkg/core/tun_client.go` | Yes |
| `TunConfigService` proto | `pkg/daemon/rpc` | Yes (same gRPC API) |
| `TunConfigServer` | `pkg/controlplane` | Yes (same server-side instance) |
| `GetTunIP` (allocate at startup) | `pkg/controlplane` | Yes (same RPC) |
| `WatchTunIP` (subscribe to changes) | `pkg/controlplane` | Client subscribes; sidecar relies on envoy xDS instead |

### 9. File Changes

| File | Operation | Description |
|------|-----------|-------------|
| `pkg/handler/network.go` | Modify | Add `ChangeTunIP`, `StartIPWatcher` |
| `pkg/handler/connect.go` | Modify | Start watcher after DoConnect completes |
| `pkg/daemon/action/connect_elevate.go` | Modify | User Daemon passes OwnerID via req; no longer allocates IP |
| `pkg/daemon/action/connect.go` | Modify | Root Daemon reads OwnerID from req; starts IP watcher |
| `pkg/daemon/action/persistence.go` | Modify | Add getSudoTunIPs/resolveTunIP helpers |
| `pkg/daemon/action/proxy.go` | Modify | Query sudo daemon IP for CreateRemoteInboundPod |
| `pkg/daemon/action/leave.go` | Modify | Query sudo daemon IP for LeaveResource |

### 10. Backward Compatibility

- If TunConfigService is unavailable (old traffic manager version), fall back to direct DHCP mode
- Root Daemon watcher connection failure does not affect normal TUN operation (only hot-update is unsupported)
- Old and new clients can connect to the same traffic manager (old clients do not call TunConfigService and allocate IPs via ConfigMap DHCP)

### 11. Known Issue Fix

#### WatchTunIP Long-Lived Connection Lease Expiry (fixed)

In the original design, the `WatchTunIP` stream did not refresh `LastRenew`. After 5 minutes, the LeaseReaper would incorrectly reclaim the IP. This has been fixed by adding an implicit lease renewal ticker on the server side within `WatchTunIP` (refreshes every `LeaseDuration/3`).
