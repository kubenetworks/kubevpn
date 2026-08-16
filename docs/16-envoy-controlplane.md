# Envoy xDS Control Plane

## 1. Overview

The control plane (`pkg/xds`) runs inside the traffic manager pod and serves as the configuration hub for all envoy sidecar proxies in the cluster. It watches the `kubevpn-traffic-manager` ConfigMap for Virtual/Rule configurations and converts them into envoy xDS snapshots that are pushed to sidecar proxies via gRPC.

The same gRPC server also hosts the `TunConfigService` (documented in `03-dhcp-ip-allocation.md`) for TUN IP allocation.

## 2. Architecture

```
ConfigMap (ENVOY_CONFIG key)
    │ K8s informer watch
    ▼
  Watcher ──── NotifyMessage{Content} ────▶ notifyCh
                                               │
                                               ▼
                                          Processor
                                          ├── parseYaml → []*Virtual
                                          ├── Virtual.To() → xDS resources
                                          └── cache.SetSnapshot(nodeID, snapshot)
                                               │
                                               ▼
                                     go-control-plane SnapshotCache
                                               │ ADS gRPC stream
                                               ▼
                                     Envoy sidecar proxies
```

## 3. Data Model

### 3.1 Virtual

A `Virtual` represents the envoy xDS configuration for a single proxied workload. Stored as YAML array in the ConfigMap's `ENVOY_CONFIG` key.

```go
type Virtual struct {
    SchemaVersion int             // config schema revision (current: CurrentSchemaVersion = 2, zero = legacy; v2 requires OwnerID on all rules)
    Namespace     string          // workload namespace
    UID           string          // group.resource.name identifier
    FargateMode   bool            // explicit fargate mode flag
    Ports         []ContainerPort // workload ports
    Rules         []*Rule         // header-based routing rules
}
```

**SchemaVersion**: Tracks breaking changes to the Virtual/Rule layout. `CurrentSchemaVersion = 2` requires OwnerID on all rules. Zero means legacy pre-versioning config.

### 3.2 ContainerPort

```go
type ContainerPort struct {
    Name              string          // IANA service name (optional)
    EnvoyListenerPort int32           // envoy bind port in fargate mode (0 in mesh mode)
    ContainerPort     int32           // original container port
    Protocol          corev1.Protocol // TCP, UDP, or SCTP
}
```

### 3.3 Rule

A `Rule` defines a header-based routing rule for traffic splitting between users:

```go
type Rule struct {
    Headers      map[string]string // header match criteria
    LocalTunIPv4 string            // destination TUN IP (IPv4)
    LocalTunIPv6 string            // destination TUN IP (IPv6)
    OwnerID      string            // connection UUID prefix (required since v2)
    PortMap      map[int32]string  // containerPort → "envoyPort" or "envoyPort:localPort"
}
```

### 3.4 PortMapping

Parsed representation of the `PortMap` string encoding, accessed via `rule.ParsePortMap()`:

```go
type PortMapping struct {
    ContainerPort int32 // original container port (map key)
    EnvoyPort     int32 // port envoy forwards to
    LocalPort     int32 // port the local machine listens on
}
```

Format: `"envoyPort"` (mesh mode, LocalPort defaults to ContainerPort) or `"envoyPort:localPort"` (fargate mode).

### 3.5 Fargate Mode Detection

`Virtual.IsFargateMode()` checks the explicit `FargateMode` bool field first, then falls back to the legacy heuristic (`EnvoyListenerPort != 0`) for backward compatibility.

## 4. Components

### 4.1 Watcher (`watcher.go`)

Monitors the `kubevpn-traffic-manager` ConfigMap using a filtered K8s informer:

- **Filtered informer**: Watches only the single ConfigMap by name via `FieldSelector`
- **Event coalescing**: On any Add/Update/Delete event, resets a 5-second ticker to fire immediately. This batches rapid changes.
- **Tick loop**: Every tick (5s default, reset on events), reads the ConfigMap from the informer's local cache and sends `NotifyMessage{Content: data[ENVOY_CONFIG]}` to the notify channel.
- **DHCP callback**: Also invokes `OnDHCPChange` callbacks on every ConfigMap change, used by TunConfigServer's `ReconcileDHCP` to detect external IP releases.

### 4.2 Processor (`processor.go`)

Converts YAML Virtual configs into envoy xDS snapshots:

```
ProcessFile(NotifyMessage):
  1. parseYaml(content) → []*Virtual
  2. For each Virtual with non-empty UID:
     a. Check expireCache — skip if unchanged (5-min TTL)
     b. Virtual.To(enableIPv6) → listeners, clusters, routes, endpoints
     c. cache.NewSnapshot(version, resources) → validate consistency
     d. cache.SetSnapshot(nodeID, snapshot) — push to go-control-plane
     e. Update expireCache
  3. Clear stale snapshots for UIDs no longer in config (knownUIDs tracking)
```

**Node ID**: Generated by `util.GenEnvoyUID(namespace, uid)` — this must match the node ID that the envoy sidecar advertises when connecting.

**Version**: Monotonically increasing int64, used by go-control-plane to detect config changes.

**Stale cleanup**: The Processor tracks `knownUIDs`. When a Virtual is removed from the ConfigMap (e.g., user leaves), its snapshot is cleared so envoy stops receiving that config.

### 4.3 xDS Resource Generation (`cache.go`)

`Virtual.To()` generates four types of envoy xDS resources:

**For each port × rule combination:**

| Resource | Name Pattern | Description |
|----------|-------------|-------------|
| Listener | `{ns}_{uid}_{port}_{proto}` | Inbound listener on the container port (or envoy port in fargate mode) |
| Route | same as listener | Route config with header-matching routes + default fallback |
| Cluster | `{tunIP}_{envoyPort}` | EDS cluster pointing to the user's TUN IP |
| Endpoint | `{tunIP}_{envoyPort}` | Load assignment: `tunIP:envoyPort` |

**TCP listener configuration:**
- `BindToPort`: true in fargate mode (envoy binds directly), false in mesh mode (uses iptables redirect)
- `UseOriginalDst`: true — preserves the original destination for ORIGINAL_DST cluster
- Listener filters: `HttpInspector` (detect HTTP) + `OriginalDestination`
- Filter chains: HTTP connection manager (with gRPC-Web, CORS, Router filters) + TCP proxy fallback

**UDP listener configuration (`toUDPListener`):**

For each UDP container port, `Virtual.To()` also emits a dedicated UDP listener per IP family. The IPv4 listener is named `{ns}_{uid}_{port}_UDP` (bound `0.0.0.0`); the IPv6 listener is named `{ns}_{uid}_{port}_UDP_v6` (bound `::`). The name and bind address **must** differ per family — a shared name collapses the two listeners to one in the xDS snapshot (last-wins), leaving the survivor bound to the wrong family so the listener never serves the requested family (observed in-cluster as the pod refusing UDP on the port). An unpopulated IP family (e.g. empty `LocalTunIPv6`) is skipped entirely rather than emitted as a listener with an invalid empty-address endpoint.
- `SocketAddress.Protocol = UDP`, bound to the container port (`BindToPort`-style, no `use_original_dst` — UDP has no `SO_ORIGINAL_DST`)
- Single listener filter `envoy.filters.udp_listener.udp_proxy` (`UdpProxyConfig`) routing to the **same cluster/endpoint as the TCP path** ({tunIP}_{envoyPort})
- No header matching — UDP has no headers, so all UDP datagrams go to the user's TUN IP. This is an inherent envoy limitation and matches pre-existing behavior (the old VPN sidecar also could not header-split UDP).
- IP hot-update works the same as TCP: the shared cluster/endpoint is updated via xDS when `Rule.LocalTunIPv4` changes.

The `udp_proxy` v3 proto is vendored under `vendor/github.com/envoyproxy/go-control-plane/envoy/extensions/filters/udp/udp_proxy/v3`.

The data path is verified end-to-end against a **real Envoy** in `pkg/xds/envoy_e2e_test.go`. Each test feeds a `Virtual` through `Virtual.To()` → snapshot cache → xDS gRPC server to a containerized Envoy, then pushes real traffic through Envoy and asserts the result:

| Test | Verifies |
|---|---|
| `TestIntegration_UDP_EnvoyEndToEnd` | UDP datagram → `udp_proxy` → upstream echo → reply |
| `TestIntegration_TCP_EnvoyEndToEnd` | HTTP request → header-matched route → upstream |
| `TestIntegration_TCP_HeaderSplit_EnvoyEndToEnd` | multi-rule header split (a→A, b→B, none→default) |
| `TestIntegration_TCP_EndpointHotUpdate_EnvoyEndToEnd` | live snapshot re-push reroutes endpoint (TUN-IP hot-update) with no restart |
| `TestIntegration_TCP_RuleTeardown_EnvoyEndToEnd` | removing one rule → its header falls back to default; sibling rule untouched |
| `TestIntegration_MixedTCPUDP_EnvoyEndToEnd` | one `Virtual` with both a TCP and a UDP port — both listeners serve |
| `TestIntegration_BootstrapFiles_EnvoyEndToEnd` | each production bootstrap (`pkg/inject/{envoy,envoy_ipv4,fargate_envoy,fargate_envoy_ipv4}.yaml`) boots Envoy, connects to xDS (`:9002`), and serves traffic |

The bootstrap-file test renders each embedded sidecar bootstrap exactly as `inject.renderEnvoyConfig` does and runs it against a real Envoy. It caught that those files carried `rds_config`/`eds_config` under `dynamic_resources` — fields that do not exist in `envoy.config.bootstrap.v3.Bootstrap.DynamicResources` (RDS comes from the HTTP connection manager, EDS from the cluster, both over ADS), which Envoy v1.35+ rejects with "unknown fields"; they were removed.

**Mesh-mode inbound capture listener:** in mesh mode the per-port TCP listeners are `BindToPort=false` (they cannot bind the app's own port), so `Virtual.To()` also emits a single virtual-inbound listener bound on `config.PortEnvoyInbound` (`:15006`) — the port the injected sidecar's iptables DNATs all inbound TCP to. It restores the original destination (`original_dst` listener filter + `use_original_dst`) and redirects to the matching per-port listener; ports without a per-port listener fall through to a passthrough filter chain that TCP-proxies to `origin_cluster` (`ORIGINAL_DST`) so the app still receives them. Without this listener, DNAT'd connections hit a closed `:15006` and the kernel resets them (surfacing to the client as "connection reset by peer"). Fargate binds each listener directly, so no capture listener is emitted. `TestVirtual_To_MeshInboundCaptureListener` (config) and `TestIntegration_MeshInboundCapture_EnvoyEndToEnd` (real Envoy binds `:15006`) lock this in; the `original_dst` redirect itself needs real iptables, so full mesh routing is only verifiable in-cluster.

The TCP tests use Fargate mode because the mesh-mode TCP listener sets `BindToPort=false` (it relies on iptables `original_dst`); the routing code path is otherwise identical. `Virtual.To()` emits one listener+cluster **per IP family** with family-distinct names and bind addresses (see the UDP listener section above), and skips an unpopulated IP family; `TestVirtual_To_UDPDualStack` and `TestVirtual_To_UDPSkipsEmptyIPv6` lock in that behavior. All tests skip gracefully when Docker is unavailable (override the image for restricted networks via `KUBEVPN_E2E_ENVOY_IMAGE`, using a native-arch Envoy — an emulated Envoy can silently drop UDP).

**Routing logic (TCP):**
- Header-matched routes → specific cluster (user's TUN IP + envoy port)
- Default route → `loopback_<containerPort>` (STATIC, `127.0.0.1:<containerPort>`, forwards to the real app) in mesh mode
- Default route → user's TUN IP + container port in fargate mode (no ORIGINAL_DST available)

**Loopback cluster (mesh declared ports)**: the no-header-match return path for a declared port goes to `127.0.0.1:<containerPort>` via a per-port STATIC cluster, not `ORIGINAL_DST → podIP`. Loopback is hard-routed to `lo` by every kernel, so the return connection never re-enters the sidecar's `PREROUTING` DNAT — this avoids the ORIGINAL_DST loop on lima/colima kernels and aligns with Istio. See [42-origin-loopback-cluster.md](42-origin-loopback-cluster.md).

**Origin cluster**: A shared `ORIGINAL_DST` cluster, still used in mesh mode for the `:15006` capture listener's **undeclared-port** passthrough (their port is unknown, so no static loopback cluster can be pre-built). Envoy forwards such traffic to the original destination.

> **Port-coverage difference vs. the old VPN-only sidecar.** Because per-port listeners are emitted only for declared container ports (`a.Ports`), a full-proxy (empty-headers) rule intercepts **only declared ports**. Traffic to an undeclared port reaches the `:15006` capture listener but has no per-port listener to redirect to, so it falls through the passthrough chain to `origin_cluster` — the real app, not the user's machine. The old VPN-only sidecar tunneled **all** ports. See [17-sidecar-injection.md](17-sidecar-injection.md) ("Full-proxy port coverage") for the implications and restore options.

### 4.4 gRPC Server (`server.go`)

Starts a gRPC server on port 9002 that registers:

- All xDS discovery services (ADS, EDS, CDS, RDS, LDS, SDS, RTDS) via go-control-plane
- `TunConfigService` for DHCP IP allocation (see `03-dhcp-ip-allocation.md`)

Server parameters: max concurrent streams = 1,000,000, keepalive = 15s.

## 5. ENVOY_CONFIG Write Path — `inject.VirtualStore`

All mutations to the `ENVOY_CONFIG` key of the `kubevpn-traffic-manager` ConfigMap go through a
single read-modify-write abstraction: `inject.VirtualStore` (in `pkg/inject/virtual_store.go`).

### Design

```go
// VirtualStore wraps a ConfigMapInterface and provides one optimistic
// read-modify-write loop for the ENVOY_CONFIG key.
type VirtualStore struct { mapInterface v12.ConfigMapInterface }

// Mutate performs RetryOnConflict(DefaultBackoff, Get → decode → fn → encode → Update).
// fn returns nil to signal a no-op (Update skipped).
func (s *VirtualStore) Mutate(ctx context.Context, fn MutationFunc) error
```

`Mutate` wraps `retry.RetryOnConflict(retry.DefaultBackoff, ...)`. On each attempt it:
1. GETs the ConfigMap (preserving `ResourceVersion`)
2. Unmarshals the YAML in `cm.Data[ENVOY_CONFIG]` → `[]*xds.Virtual`
3. Calls `fn` (the mutation)
4. Marshals back to YAML and calls `Update` (optimistic concurrency via `ResourceVersion`)

If the Update returns a 409 Conflict (concurrent writer bumped `ResourceVersion`), `RetryOnConflict`
retries from the Get. If `fn` returns `nil, nil`, the Update is skipped (idempotent no-op).

### Methods

| Method | Replaces | Called from |
|--------|----------|-------------|
| `VirtualStore.AddRule(ctx, envoyRuleSpec)` | `addEnvoyConfig` standalone func | `pkg/inject/envoy.go:addEnvoyConfig` (thin wrapper) |
| `VirtualStore.RemoveRule(ctx, ns, nodeID, ownerID)` | `removeEnvoyConfig` standalone func | `pkg/inject/envoy.go:removeEnvoyConfig` (thin wrapper) |

`addEnvoyConfig` and `removeEnvoyConfig` in `envoy.go` remain as the call-site names (callers in
`mesh.go` and `fargate.go` are unchanged) but their bodies now delegate to `VirtualStore`.

### syncEnvoyRuleIP — Get+Update (not Patch)

`pkg/xds/tun_config.go:syncEnvoyRuleIP` (called when a client's TUN IP changes) was previously
implemented with a JSON Patch (`k8stypes.JSONPatchType`). It has been rewritten to use the same
Get→decode→mutate→encode→Update loop with `retry.RetryOnConflict(retry.DefaultBackoff, ...)`,
matching the approach used by `addEnvoyConfig` and `removeEnvoyConfig`.

This matters because JSON Patch bypasses ResourceVersion-based optimistic locking at the field
level: two concurrent JSON Patches on the same key can silently overwrite each other (the
`retry.RetryOnConflict` wrapper had no practical effect since JSON Patch on a data field never
returns a 409). With Get+Update all three writers share the same ResourceVersion conflict-detection
mechanism.

`syncEnvoyRuleIP` does NOT import `pkg/inject` (that would create an import cycle since
`pkg/inject` already imports `pkg/xds`). It inlines the same Get→YAML→Update pattern rather than
calling through `VirtualStore`.

### Concurrency Model

`VirtualStore` does not use a serializing mutex — it relies on optimistic concurrency. Two
concurrent `Mutate` calls that both Get the same `ResourceVersion` will race to Update; one
succeeds, the other gets a 409 and retries with a fresh Get. `retry.DefaultBackoff` (exponential,
~5 retries) handles transient conflicts.

## 6. Startup Flow

`Main()` orchestrates everything:

```
Main(ctx, factory, port, logger):
  1. Create SnapshotCache + Processor
  2. Create TunConfigServer (fatal on failure) + start LeaseReaper
  3. Start gRPC server (goroutine)
  4. Start ConfigMap watcher (goroutine) → sends to notifyCh
  5. Send initial empty NotifyMessage to trigger first snapshot
  6. Main loop: receive from notifyCh → proc.ProcessFile()
```

**TunConfigServer init is a hard prerequisite, not best-effort.** `NewTunConfigServer`
runs `InitDHCP` (a ConfigMap read/create), which can transiently fail on a freshly
created traffic-manager pod whose network is not yet ready (`connect: connection refused`
to the API server ClusterIP). It therefore retries with a bounded exponential backoff
(`initDHCPBackoff`, ~1 min). If init still fails, `Main` returns the error — **fatal**,
so the xds container exits non-zero and Kubernetes restarts it. This avoids the failure
mode where the gRPC server comes up **without** `TunConfigService` registered: that
endpoint is otherwise healthy and never restarts, so every client's TUN setup fails
permanently with `unknown service rpc.TunConfigService`.

## 7. Multi-User Traffic Splitting

When multiple users proxy the same workload with different headers:

```yaml
# ConfigMap ENVOY_CONFIG (simplified)
- namespace: default
  Uid: deployments.apps.productpage
  schemaVersion: 2
  ports:
    - containerPort: 9080
      protocol: TCP
  rules:
    - headers: {"x-user": "alice"}
      localTunIPv4: "198.18.0.5"
      ownerID: "a1b2c3d4e5f6"
      portMap: {9080: "9080"}
    - headers: {"x-user": "bob"}
      localTunIPv4: "198.18.0.6"
      ownerID: "f6e5d4c3b2a1"
      portMap: {9080: "9080"}
```

Envoy routes requests with `x-user: alice` to `198.18.0.5:9080` (Alice's local machine) and `x-user: bob` to `198.18.0.6:9080` (Bob's). Unmatched requests go to `origin_cluster` (the real application).

## 8. Related Files

| File | Purpose |
|------|---------|
| `pkg/xds/xds.go` | `Main()` — startup orchestration (TunConfigServer init is fatal on failure) |
| `pkg/xds/cache.go` | Virtual/Rule types, xDS resource generation |
| `pkg/xds/processor.go` | Processor — YAML → xDS snapshot pipeline |
| `pkg/xds/watcher.go` | ConfigMap watcher with informer |
| `pkg/xds/server.go` | gRPC server setup |
| `pkg/xds/tun_config.go` | TunConfigServer (see `03-dhcp-ip-allocation.md`) |
| `pkg/inject/envoy.go` | Writes Virtual/Rule to ConfigMap (producer side); `addEnvoyConfig`/`removeEnvoyConfig` delegate to `VirtualStore` |
| `pkg/inject/virtual_store.go` | `VirtualStore` — single read-modify-write path for `ENVOY_CONFIG` (`Mutate`, `AddRule`, `RemoveRule`) |
| `pkg/config/config.go` | ConfigMap key names (`KeyEnvoy`, etc.) |
