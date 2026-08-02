# Unified Proxy Mode and TUN IP Hot-Update Design

## 1. Problem

### 1.1 Scenario

After a user runs `kubevpn proxy deploy/web`, the laptop sleeps for more than 5 minutes, the DHCP lease
expires, and the TUN IP is reclaimed. After the laptop wakes, the client obtains a new TUN IP, but the
cluster-side routing rules (envoy xDS config / VPN sidecar iptables DNAT) still point at the old IP,
breaking traffic.

### 1.2 Detailed Timeline

```
T0: kubevpn proxy deploy/web --headers version=v1
    ┌─ User Daemon ──────────────────────────────────────────────────────┐
    │ getSudoTunIPs() → tunV4 = "198.18.0.5"                            │
    │ CreateRemoteInboundPod(tunV4="198.18.0.5", ownerID="abc123")       │
    │   → addEnvoyConfig: Rule{LocalTunIPv4:"198.18.0.5", OwnerID:"abc"}│
    │   → AddVPNContainer: env LocalTunIPv4=198.18.0.5                   │
    │     → sidecar startup script: iptables DNAT --to 198.18.0.5        │
    └────────────────────────────────────────────────────────────────────┘
    ┌─ Root Daemon ──────────────────────────────────────────────────────┐
    │ Start() → portForward() → startTUN() → rentIP()                    │
    │   → GetTunIP(ownerID="abc123") → allocate 198.18.0.5               │
    │   → TUN device created: 198.18.0.5                                 │
    │ StartIPWatcher() → WatchTunIP(ownerID="abc123") stream established  │
    │ heartbeats() → ICMP src=198.18.0.5 → RouteHub registers 198.18.0.5 │
    └────────────────────────────────────────────────────────────────────┘
    ┌─ Traffic Manager ──────────────────────────────────────────────────┐
    │ allocs["abc123"] = {IPv4: 198.18.0.5, LastRenew: T0}               │
    │ ConfigMap ENVOY_CONFIG: Rule{LocalTunIPv4:"198.18.0.5"}            │
    │ Processor → xDS snapshot → envoy route: version=v1 → 198.18.0.5    │
    └────────────────────────────────────────────────────────────────────┘
    ✅ traffic OK

T1: laptop sleeps (lid close / lock)
    all local processes suspended: heartbeats stop, port-forward drops, WatchTunIP drops

T1 + 5min: LeaseReaper reclaims the IP
    reapExpiredLeases → delete(allocs, "abc123") → dhcp.ReleaseIP(198.18.0.5)
    ⚠️ ConfigMap ENVOY_CONFIG's Rule.LocalTunIPv4 is still "198.18.0.5"

T2: laptop wakes
    port-forward reconnects → starts a new healthCheckGRPC (control-plane port PortControlPlane 9002)
    healthCheckGRPC does a gRPC health Check every 30s (verifies SERVING)
    → verifies the port-forward + control plane are alive
    → does NOT call GetTunIP, does NOT trigger IP reallocation

    IPWatcher.doWatchTunIP reconnects the WatchTunIP stream
    → WatchTunIP only pushes on NotifyIPChange
    → ⚠️ GetTunIP's new allocation does not call NotifyIPChange → the stream never receives a push

T2 + 30s: traffic broken
    local TUN:      198.18.0.5 (old)
    server allocs:  198.18.0.7 (new, reallocated by the health check)
    ENVOY_CONFIG:   198.18.0.5 (old, never updated)
    sidecar DNAT:   198.18.0.5 (old, hard-coded at startup)

    ⚠️ If another user now gets 198.18.0.5 → route conflict → traffic cross-wiring
```

### 1.3 Impact

| Mode | Affected | Break point |
|------|--------|--------|
| **VPN-only** | ❌ Yes | sidecar `iptables DNAT --to ${oldIP}` is hard-coded and not updated at runtime; the server's alloc IP and the local TUN IP diverge |
| **Mesh** | ❌ Yes | `Rule.LocalTunIPv4` in ConfigMap `ENVOY_CONFIG` is the old IP; envoy xDS routes to the old IP |
| **Fargate** | ✅ No | `LocalTunIPv4` is fixed at `127.0.0.1`, independent of the TUN IP |

### 1.4 Root-Cause Analysis

#### Problem 1 (fixed): the old healthCheckTCPConn's GetTunIP triggered silent reallocation

**Fixed**: the health check no longer calls gRPC `GetTunIP`; it uses `healthCheckGRPC` to do a gRPC health
`Check` against the control-plane port. It does not call `GetTunIP`, fully eliminating health-check-induced
silent IP reallocation. (An earlier DNS-via-gudp `healthCheckPortForward` was used; it has since been
deleted as dead code.)

#### Problem 2: GetTunIP does not push on WatchTunIP when allocating a new IP

**Location**: `pkg/xds/tun_config.go:227-250` (new-allocation path)

```go
s.allocs[req.OwnerID] = alloc
go s.saveAllocs(context.Background())
return &rpc.TunIPResponse{...}, nil  // ← does not call NotifyIPChange
```

`ReconcileDHCP` **does call** `NotifyIPChange` when the IP changes, but neither GetTunIP's new-allocation
nor reallocation path does. The `WatchTunIP` stream receives no push, `ChangeTunIP` is never called, and a
divergent local TUN IP **cannot self-heal**.

#### Problem 3: ConfigMap ENVOY_CONFIG and the sidecar iptables are unaware of IP changes

Even if problems 1 and 2 are fixed (the local TUN IP updates correctly), two downstream components are
still unaware of the change:

| Component | Problem | Location |
|------|------|------|
| `Rule.LocalTunIPv4` in ConfigMap `ENVOY_CONFIG` | written at inject time and never updated; envoy xDS routes to the old IP | `inject/envoy.go` addEnvoyConfig is only called at inject time |
| VPN sidecar iptables DNAT | the container startup script hard-codes `DNAT --to ${LocalTunIPv4}` and never updates at runtime | `inject/container.go:191` |

**Multi-user safety risk**: after user A wakes, its traffic still uses the old IP 198.18.0.5, but the
server thinks A's IP is 198.18.0.7. If user B now gets 198.18.0.5, the RouteHub entries conflict and
**traffic cross-wires**.

---

## 2. ENVOY_CONFIG Format

### 2.1 Storage Location

The routing config is stored in the `ENVOY_CONFIG` key (`config.KeyEnvoy`) of the ConfigMap
`kubevpn-traffic-manager`, as a YAML-serialized `[]*Virtual` array.

**Before** (current code):

```yaml
# ConfigMap: kubevpn-traffic-manager (before)
DHCP: "base64..."          # IPv4 bitmap in its own key (misnamed — not the real DHCP protocol)
DHCP6: "base64..."         # IPv6 bitmap in its own key (redundant split)
IPv4_POOLS: "10.96.0.0/12 10.244.0.0/16 fd00::/56"  # named IPv4 but actually contains IPv6 (misnamed)
TUN_ALLOCS: |              # ...
ENVOY_CONFIG: |            # ...
```

**After**: `DHCP`+`DHCP6` merged into `TUN_IP_POOL`, `IPv4_POOLS` renamed to `CLUSTER_CIDRS`:

```yaml
# ConfigMap: kubevpn-traffic-manager (after)

TUN_IP_POOL: |                         # TUN IP pool (merges the old DHCP + DHCP6)
  ipv4:                                #   bitmap managed via cilium/ipam ContiguousAllocationMap
    cidr: 198.18.0.0/16                #   RentIP/ReleaseIP operate on v4+v6 together
    bitmap: "base64-encoded..."        #   65534 usable addresses
  ipv6:
    cidr: 2001:2::/64
    bitmap: "base64-encoded..."

TUN_ALLOCS: |                          # TUN IP allocation map (ownerID → dual-stack IP)
  a1b2c3d4e5f6:                        #   ownerID = UUID[:12] (client) or podName (sidecar)
    ipv4: 198.18.0.5/32
    ipv6: 2001:2::5/128
    version: 1717900000000000000
    lastRenew: 1717900120
  f6e5d4c3b2a1:
    ipv4: 198.18.0.6/32
    ipv6: 2001:2::6/128
    version: 1717900100000000000
    lastRenew: 1717900200

CLUSTER_CIDRS: "10.96.0.0/12 10.244.0.0/16 fd00:10:244::/56"
                                       # cluster CIDR cache (renamed from IPv4_POOLS)
                                       #   space-separated CIDR string (encodeCIDRs/parseCachedCIDRs)
                                       #   contains Service CIDR + Pod CIDR (IPv4 + IPv6)
                                       #   detected from: kube-controller-manager / CNI IPAM / Node PodCIDR
                                       #   used to configure the local route table (which CIDRs go via the TUN tunnel)

ENVOY_CONFIG: |                        # envoy routing config ← the focus of this document
  - schemaVersion: 2
    Uid: deployments.apps.web
    ...
```

**ConfigMap data-structure changes**:

| Item | Before | After | Files involved |
|--------|--------|--------|---------|
| TUN IP pool | `DHCP` (v4) + `DHCP6` (v6), two keys, each base64 | merged into one `TUN_IP_POOL` key, a YAML struct with `ipv4.bitmap` + `ipv6.bitmap` | `pkg/config/config.go`, `pkg/dhcp/dhcp.go`, `pkg/handler/traffmgr.go` |
| Cluster CIDR | `IPv4_POOLS` (misnamed, actually contains v6) | `CLUSTER_CIDRS`, space-separated CIDR string | `pkg/config/config.go`, `pkg/handler/connect.go`, `pkg/handler/traffmgr.go`, `pkg/dhcp/dhcp.go` |

### 2.2 Data Model

```
ENVOY_CONFIG ([]*Virtual)
│
├── Virtual[0]                          ← one proxied workload
│   ├── SchemaVersion: 2                ← config version (current CurrentSchemaVersion=2, requires OwnerID)
│   ├── UID: "deployments.apps.web"     ← workload identity (group.resource.name)
│   ├── Namespace: "default"            ← workload namespace
│   ├── FargateMode: false              ← whether Fargate/Service mode
│   ├── Ports:                          ← ports exposed by the workload
│   │   └── [{ContainerPort: 8080, Protocol: TCP, EnvoyListenerPort: 0}]
│   └── Rules:                          ← routing rules (one per proxy user)
│       ├── Rule[0]                     ← user A's rule
│       │   ├── Headers: {version: v1}  ← header match (empty = full hijack)
│       │   ├── LocalTunIPv4: "198.18.0.5"  ← user A's TUN IP (envoy route target)
│       │   ├── LocalTunIPv6: "2001:2::5"
│       │   ├── OwnerID: "a1b2c3d4e5f6"    ← user A's connection UUID[:12]
│       │   └── PortMap: {8080: "9080"}     ← port mapping
│       └── Rule[1]                     ← user B's rule
│           ├── Headers: {version: v2}
│           ├── LocalTunIPv4: "198.18.0.9"
│           ├── OwnerID: "f6e5d4c3b2a1"
│           └── PortMap: {8080: "9080"}
│
├── Virtual[1]                          ← another proxied workload
│   └── ...
```

**YAML example**:

```yaml
- schemaVersion: 2
  Uid: deployments.apps.web
  namespace: default
  ports:
  - containerPort: 8080
    protocol: TCP
  rules:
  - headers:
      version: v1
    localtunipv4: "198.18.0.5"
    localtunipv6: "2001:2::5"
    ownerID: "a1b2c3d4e5f6"
    portmap:
      8080: "9080"
  - headers:
      version: v2
    localtunipv4: "198.18.0.9"
    localtunipv6: "2001:2::9"
    ownerID: "f6e5d4c3b2a1"
    portmap:
      8080: "9080"
```

### 2.3 Key Fields

| Field | Description |
|------|------|
| `SchemaVersion` | config version. 0 = legacy (no OwnerID); 2 = current (OwnerID required) |
| `UID` | workload identity, format `group.resource.name`, e.g. `deployments.apps.web`. Used by `util.GenEnvoyUID(namespace, uid)` to generate the xDS nodeID |
| `FargateMode` | when `true`, envoy uses `BindToPort=true` (binds the port directly) and the Service targetPort is modified; when `false`, uses `use_original_dst` (iptables redirect) |
| `Ports[].ContainerPort` | the original container port |
| `Ports[].EnvoyListenerPort` | the random port envoy binds in Fargate mode (0 when not Fargate) |
| `Rules[].Headers` | envoy header routing match. **Empty matches all requests** (VPN-only full hijack) |
| `Rules[].LocalTunIPv4/v6` | **the envoy route target IP** — the IP of the user's local TUN device. **The core field this document fixes**: it must auto-update when the IP changes |
| `Rules[].OwnerID` | connection identity (UUID[:12]), used for rule matching and deletion. Generated at inject time, the Rule's primary key |
| `Rules[].PortMap` | port mapping. Format `containerPort → "envoyPort"` or `containerPort → "envoyPort:localPort"` (Fargate mode) |

### 2.4 Rule Write / Update / Delete

**Write** — `addEnvoyConfig(ctx, configMapInterface, envoyRuleSpec)` (`pkg/inject/envoy.go`):

`addVirtualRule` handles four cases:

| Case | Condition | Action |
|------|------|------|
| 1 | new workload (no matching Virtual) | create Virtual + Rule |
| 2 | same user updating (OwnerID matches) | update LocalTunIPv4/v6, merge Headers/PortMap |
| 3 | different user, same headers (header match) | replace OwnerID and IP (header ownership transfer) |
| 4 | new user, different headers | append Rule |

**New in this design**: `syncEnvoyRuleIP` reuses the **Case 2** logic — when the OwnerID matches it
auto-updates `LocalTunIPv4/v6`, requiring no extra code.

**Delete** — `removeEnvoyConfig(ctx, configMapInterface, namespace, nodeID, ownerID)`:

deletes the matching Rule by OwnerID. If the Virtual's last Rule is deleted, the whole Virtual entry is removed.

### 2.5 ENVOY_CONFIG → xDS Push Path

```
addEnvoyConfig / syncEnvoyRuleIP
  → update ConfigMap ENVOY_CONFIG
  → Watcher (K8s informer, pkg/xds/watcher.go)
    → detects the ConfigMap change
    → notifyCh <- NotifyMessage{Content: configMap.Data["ENVOY_CONFIG"]}
  → Processor (pkg/xds/processor.go)
    → parseYaml(content) → []*Virtual
    → for each Virtual:
      → nodeID = GenEnvoyUID(namespace, uid)
      → check expireCache (5min TTL, skip unchanged config)
      → Virtual.To(enableIPv6) → generate xDS resources:
        ├── Listener (TCP: use_original_dst / Fargate: BindToPort / UDP: udp_proxy)
        ├── Route    (header match → specific cluster / default → origin_cluster)
        ├── Cluster  ({tunIP}_{envoyPort})
        └── Endpoint ({tunIP}:{envoyPort} — LocalTunIPv4 is consumed here)
      → cache.SetSnapshot(nodeID, snapshot)
  → envoy sidecar
    → ADS gRPC stream (connected to the :9002 xDS server)
    → receives the new snapshot → routing hot-update
```

**Key**: `LocalTunIPv4` is ultimately turned into the envoy endpoint address in `toEndPoint`. IP change →
ENVOY_CONFIG update → Processor generates a new endpoint → xDS push → envoy routes to the new IP. This is
the existing path this design leverages.

### 2.6 Multi-user Scenario

```yaml
# user A (version=v1) and user B (version=v2) proxy deploy/web at the same time
- schemaVersion: 2
  Uid: deployments.apps.web
  namespace: default
  ports:
  - containerPort: 9080
    protocol: TCP
  rules:
  - headers: {x-user: alice}
    localtunipv4: "198.18.0.5"
    ownerID: "a1b2c3d4e5f6"
    portmap: {9080: "9080"}
  - headers: {x-user: bob}
    localtunipv4: "198.18.0.6"
    ownerID: "f6e5d4c3b2a1"
    portmap: {9080: "9080"}
```

envoy routing:
- `x-user: alice` → `198.18.0.5:9080` (Alice's machine)
- `x-user: bob` → `198.18.0.6:9080` (Bob's machine)
- no match → `origin_cluster` (ORIGINAL_DST, back to the origin service)

After Alice's machine wakes and its IP becomes `198.18.0.7`: `syncEnvoyRuleIP` updates
`Rule[0].LocalTunIPv4 = "198.18.0.7"` → xDS push → envoy automatically routes to the new IP. Bob is unaffected.

---

## 3. Design Goals

1. **Auto-update** ENVOY_CONFIG when the IP changes (server-initiated push, not client polling)
2. The client (Root Daemon) detects the IP change via the **fixed WatchTunIP push path** and auto-updates the local TUN device
3. **Unify the VPN-only and Mesh injection strategies**: eliminate `vpnInjector`; VPN-only = a Mesh with empty headers, all routed through envoy (TCP + UDP)
4. Routing config changes in all modes are **hot-updated automatically via xDS push**, fully aligned with envoy
5. **Belt and suspenders**: `NotifyIPChange` event push (real-time) + `WatchTunIP` ticker periodic push (fallback)

---

## 3. Overview

```
                            laptop wakes
                               │
                    Root Daemon reconnects
                               │
               healthCheck → GetTunIP(ownerID) → new IP 198.18.0.7
                               │
              TunConfigServer detects the IP change
                               │
         ┌─────────────────────┼───────────────────────────┐
         ▼                     ▼                           ▼
   update allocs         update ConfigMap            notifyWatchers
   (in memory)           ENVOY_CONFIG                → WatchTunIP push
                         Rule.LocalTunIPv4                 │
                               │                    Root Daemon:
         ┌─────────────────────┤                    ChangeTunIP
         ▼                     ▼                    local TUN updated ✅
   Watcher detects        Processor
   ConfigMap change       new xDS snapshot   ┌──────────────────┐
         │                     │             │ periodic fallback │
         ▼                     ▼             │ (~100s)           │
   envoy sidecar         TCP+UDP routes      │ WatchTunIP ticker │
   ADS push              all hot-updated ✅   │ re-pushes current │
                                             └──────────────────┘
```

### Three fix layers

| Layer | What it fixes | Problem solved |
|------|---------|-----------|
| **Step 0** | `GetTunIP` calls `notifyWatchers` when allocating a new IP | Problem 2: broken WatchTunIP push path |
| **Step 0.5** | `WatchTunIP` ticker periodically pushes the current IP | fallback: events missed during stream reconnect |
| **Step 1** | `GetTunIP` calls `syncEnvoyRuleIP` to update the ConfigMap when the IP changes | Problem 3: ENVOY_CONFIG unaware of the change |
| **Step 2** | unify VPN-only/Mesh → all via envoy (TCP+UDP) | Problem 3: eliminate the iptables DNAT hard-coding |

---

## 4. Detailed Design

### 4.1 Step 0: GetTunIP calls notifyWatchers when allocating a new IP

**Location**: `pkg/xds/tun_config.go` — `GetTunIP`

Currently GetTunIP's new-allocation and reallocation paths return immediately after allocating a new IP,
without notifying the `WatchTunIP` stream. Fix: call `notifyWatchers` after allocating the new IP.

**New-allocation path (line 227-250)**:

```go
// Before:
s.allocs[req.OwnerID] = alloc
go s.saveAllocs(context.Background())
return &rpc.TunIPResponse{...}, nil

// After:
s.allocs[req.OwnerID] = alloc
s.saveAllocs(ctx)  // synchronous, no longer fire-and-forget

// Notify the WatchTunIP stream (IPWatcher will call ChangeTunIP to update the local TUN)
s.notifyWatchers(req.OwnerID, &rpc.TunIPResponse{
    IPv4: alloc.IPv4.String(), IPv6: alloc.IPv6.String(), Version: alloc.Version,
})

// Synchronously update ENVOY_CONFIG (see Step 1)
go s.syncEnvoyRuleIP(context.Background(), req.OwnerID, alloc.IPv4, alloc.IPv6)

return &rpc.TunIPResponse{...}, nil
```

**Reallocation path (line 188-224)**: likewise add `notifyWatchers` + `syncEnvoyRuleIP` after
`s.allocs[req.OwnerID] = newAlloc`.

**`notifyWatchers` extracted into its own method** (the push logic previously scattered in `NotifyIPChange`):

```go
func (s *TunConfigServer) notifyWatchers(ownerID string, resp *rpc.TunIPResponse) {
    for _, ch := range s.watchers[ownerID] {
        select {
        case ch <- resp:
        default:
        }
    }
}
```

### 4.2 Step 0.5: WatchTunIP periodically pushes the current IP (fallback)

**Location**: `pkg/xds/tun_config.go` — `WatchTunIP`

Currently the `WatchTunIP` ticker only renews the lease every `LeaseDuration/3` (~100s). Extend it to renew
+ push the current IP:

```go
case <-ticker.C:
    s.mu.Lock()
    s.renewLease(req.OwnerID)
    // fallback: periodically push the current IP
    if alloc, ok := s.allocs[req.OwnerID]; ok {
        resp := &rpc.TunIPResponse{Version: alloc.Version}
        if alloc.IPv4 != nil {
            resp.IPv4 = alloc.IPv4.String()
        }
        if alloc.IPv6 != nil {
            resp.IPv6 = alloc.IPv6.String()
        }
        select {
        case ch <- resp:
        default:
        }
    }
    s.mu.Unlock()
```

The client-side `doWatchTunIP` already compares versions (`resp.Version != *currentVersion`), so it does
not trigger `ChangeTunIP` when the IP is unchanged — zero side effects.

**Belt and suspenders**:

| Mechanism | Trigger | Latency | Covered scenario |
|------|---------|------|---------|
| `notifyWatchers` event push | when `GetTunIP` allocates a new IP | milliseconds | real-time when the stream is alive |
| ticker periodic push | every ~100s | up to 100s | re-push after stream reconnect / recovery from a lost push |

### 4.3 Step 1: GetTunIP auto-updates ENVOY_CONFIG server-side when the IP changes

**Location**: `pkg/xds/tun_config.go` — new `syncEnvoyRuleIP`

When GetTunIP allocates an IP different from the previous one, look up the matching Rule in ENVOY_CONFIG by
ownerID and update `LocalTunIPv4/v6`.

```go
func (s *TunConfigServer) syncEnvoyRuleIP(ctx context.Context, ownerID string, newIPv4, newIPv6 *net.IPNet) {
    err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
        cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(
            ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
        if err != nil {
            return err
        }
        virtuals, err := parseYaml(cm.Data[config.KeyEnvoy])
        if err != nil {
            return err
        }

        changed := false
        for _, v := range virtuals {
            for _, rule := range v.Rules {
                if rule.OwnerID == ownerID && rule.LocalTunIPv4 != newIPv4.IP.String() {
                    rule.LocalTunIPv4 = newIPv4.IP.String()
                    if newIPv6 != nil {
                        rule.LocalTunIPv6 = newIPv6.IP.String()
                    }
                    changed = true
                }
            }
        }
        if !changed {
            return nil
        }

        data, err := yaml.Marshal(virtuals)
        if err != nil {
            return err
        }
        cm.Data[config.KeyEnvoy] = string(data)
        _, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
        return err
    })
    if err != nil {
        plog.G(ctx).Errorf("[TunConfig] syncEnvoyRuleIP failed for owner %s: %v", ownerID, err)
    }
}
```

After the ConfigMap update, the existing path takes effect automatically:

```
ConfigMap change → Watcher (informer) → Processor.ProcessFile
→ Virtual.To() → xDS resources → cache.SetSnapshot(nodeID)
→ envoy sidecar ADS push → routing hot-update ✅
```

**Mesh mode is fully fixed at this step**, with zero extra changes.

### 4.4 Step 2: Unify VPN-only and Mesh — everything via envoy

#### 4.4.1 Rationale

VPN-only mode can be viewed as **a Mesh mode with empty headers**.

The envoy route generated by `toRoute(clusterName, emptyHeaders)` is `RouteMatch{Prefix: "/", Headers: nil}`
— it matches all requests (no header constraint), equivalent to a full hijack.

envoy supports UDP proxy (`envoy.extensions.filters.udp.udp_proxy.v3.UdpProxyConfig`), routing UDP traffic
via `cluster → endpoint` with the same xDS push model as TCP. So both TCP and UDP can be forwarded through
envoy.

**After unification**:

```
Before:
  VPN-only:  iptables DNAT → ${LocalTunIPv4}  ← hard-coded, TCP+UDP, not hot-updatable
  Mesh:      iptables DNAT → :15006 (envoy)    ← fixed port, TCP only, xDS hot-update

After:
  unified Mesh: iptables DNAT → :15006 (envoy)  ← fixed port, TCP+UDP, xDS hot-update
                empty headers → envoy matches everything → full forward (former VPN-only behavior)
                non-empty headers → envoy header routing → split (former Mesh behavior)
```

**Result**: eliminate `vpnInjector`, delete `AddVPNContainer`. All proxy modes go through `meshInjector`
(VPN + envoy sidecar).

#### 4.4.2 TCP vs UDP Routing Differences

TCP and UDP route differently in envoy:

**TCP** (existing, unchanged):
- `use_original_dst=true`: envoy uses the `SO_ORIGINAL_DST` socket option to recover the original target before the iptables DNAT
- header match → forward to the user IP; no match → `origin_cluster` (`ORIGINAL_DST` cluster) back to the origin service
- `origin_cluster` relies on `SO_ORIGINAL_DST`, valid for TCP only

**UDP** (new):
- the `udp_proxy` filter specifies the upstream directly via `cluster`, and **does not support header routing** (UDP has no header concept)
- UDP does not support `SO_ORIGINAL_DST`, so `origin_cluster` cannot be used
- the UDP listener uses `BindToPort=true` to bind the container UDP port (similar to Fargate mode)

**Behavior per scenario**:

| Scenario | TCP routing | UDP routing |
|------|---------|---------|
| **VPN-only (empty headers)** | envoy route matches all → cluster → user IP ✅ | udp_proxy → same cluster → user IP ✅ |
| **Mesh (with headers)** | header match → user IP; no match → `origin_cluster` ✅ | udp_proxy → all to the user IP (UDP cannot split by header, consistent with current behavior) |

> **Note**: UDP cannot split by header under Mesh mode — this is an inherent envoy limitation. The current
> Mesh VPN sidecar (iptables DNAT → :15006) also does not support UDP header splitting, so behavior does
> not regress.

#### 4.4.3 envoy UDP listener config

In the xDS listeners generated by `Virtual.To()`, generate a UDP listener for each UDP port:

```yaml
- name: "{ns}_{uid}_{port}_UDP"
  address:
    socket_address:
      protocol: UDP
      address: "0.0.0.0"
      port_value: {containerPort}
  listener_filters:
  - name: envoy.filters.udp_listener.udp_proxy
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.filters.udp.udp_proxy.v3.UdpProxyConfig
      stat_prefix: udp_proxy
      cluster: "{tunIP}_{envoyRulePort}"  # reuse TCP's cluster → endpoint
```

The UDP listener uses `BindToPort=true` to bind the container UDP port directly, without `use_original_dst`.
cluster/endpoint are fully shared with TCP.

#### 4.4.4 `Virtual.To()` change

File: `pkg/xds/cache.go`

Alongside the existing TCP listener generation, generate a UDP listener + udp_proxy filter for UDP ports:

```go
func (a *Virtual) To(enableIPv6 bool, logger *log.Entry) (
    listeners, clusters, routes, endpoints []types.Resource,
) {
    for _, port := range a.Ports {
        // ... existing TCP listener + route + cluster + endpoint ...

        // new: UDP listener (reuses the same cluster/endpoint)
        if port.Protocol == corev1.ProtocolUDP || port.Protocol == "" {
            udpListener := toUDPListener(listenerName+"_UDP", port, clusterName)
            listeners = append(listeners, udpListener)
        }
    }
    // ...
}

func toUDPListener(name string, port ContainerPort, clusterName string) *listener.Listener {
    return &listener.Listener{
        Name: name,
        Address: &core.Address{
            Address: &core.Address_SocketAddress{
                SocketAddress: &core.SocketAddress{
                    Protocol:      core.SocketAddress_UDP,
                    Address:       "0.0.0.0",
                    PortSpecifier: &core.SocketAddress_PortValue{PortValue: uint32(port.ContainerPort)},
                },
            },
        },
        ListenerFilters: []*listener.ListenerFilter{{
            Name: "envoy.filters.udp_listener.udp_proxy",
            ConfigType: &listener.ListenerFilter_TypedConfig{
                TypedConfig: mustMarshalAny(&udpproxyv3.UdpProxyConfig{
                    StatPrefix:     "udp_proxy",
                    RouteSpecifier: &udpproxyv3.UdpProxyConfig_Cluster{Cluster: clusterName},
                }),
            },
        }},
    }
}
```

#### 4.4.5 Eliminate vpnInjector

File: `pkg/inject/injector.go`

```go
// Before: three strategies
func NewInjector(opts InjectOptions) Injector {
    if util.IsK8sService(opts.Object) {
        return &fargateInjector{opts: opts}
    }
    if len(opts.Headers) > 0 || len(opts.PortMaps) > 0 {
        return &meshInjector{opts: opts}
    }
    return &vpnInjector{opts: opts}
}

// After: two strategies
func NewInjector(opts InjectOptions) Injector {
    if util.IsK8sService(opts.Object) {
        return &fargateInjector{opts: opts}
    }
    return &meshInjector{opts: opts}  // empty headers = full hijack
}
```

**Delete** `pkg/inject/vpn.go` (`vpnInjector`) and the `AddVPNContainer` function.

#### 4.4.6 Unify iptables rules

File: `pkg/inject/container.go`

All modes use Mesh's iptables rules, no longer depending on the user IP:

```shell
# unified: DNAT to the envoy port
iptables -t nat -A PREROUTING ! -p icmp ! -s 127.0.0.1 ! -d ${CIDR4} -j DNAT --to :15006
ip6tables -t nat -A PREROUTING ! -p icmp ! -s ::1 ! -d ${CIDR6} -j DNAT --to :15006
```

The `LocalTunIPv4/LocalTunIPv6` env vars are no longer needed. After `AddVPNContainer` is deleted,
everything uses `AddVPNAndEnvoyContainer`.

#### 4.4.7 Client keeps WatchTunIP

The client's (Root Daemon) TUN IP change is delivered via the `WatchTunIP` push (Step 0 fix + Step 0.5
periodic fallback). xDS snapshots are organized by nodeID (workload dimension); the client has no
corresponding nodeID and does not use xDS.

```
sidecar routing hot-update: xDS endpoint push (reuses the ENVOY_CONFIG path)
client TUN IP hot-update:   WatchTunIP push (fix + fallback)
```

---

## 5. End-to-end Data Flow

```
laptop sleeps 5 minutes → LeaseReaper reclaims IP 198.18.0.5

laptop wakes:
  Root Daemon → port-forward reconnects
    → healthCheckGRPC does a gRPC health Check against the control-plane port
    → verifies the port-forward + control plane are alive (does NOT call GetTunIP, no IP reallocation)

  TunConfigServer.GetTunIP (server-side, in the same call):
    1. allocate 198.18.0.7, write allocs["abc123"]
    2. notifyWatchers → WatchTunIP stream push {IPv4: 198.18.0.7}
    3. syncEnvoyRuleIP → ConfigMap ENVOY_CONFIG Rule.LocalTunIPv4 = 198.18.0.7

  Root Daemon IPWatcher (WatchTunIP stream receives the push):
    → ChangeTunIP(198.18.0.7) → local TUN device updated ✅
    → heartbeat ICMP src=198.18.0.7 → RouteHub registers the new IP ✅

  envoy sidecar (unified xDS push):
    → Watcher detects the ConfigMap ENVOY_CONFIG change
    → Processor.ProcessFile → new xDS snapshot (endpoint IP=198.18.0.7)
    → cache.SetSnapshot → envoy ADS push
    → TCP routing hot-update ✅
    → UDP routing hot-update ✅ (udp_proxy, same cluster/endpoint)

  client fallback (WatchTunIP ticker ~100s):
    → even if the event push is lost, the ticker re-pushes the current IP within 100s
    → version comparison → triggers ChangeTunIP ✅

  latency:
    event push:       ~milliseconds
    periodic fallback: up to ~100s
```

---

## 5b. Manual IP override via `TUN_ALLOCS` (operator action)

`TUN_ALLOCS` is normally a **server-written crash-recovery journal** (the in-memory
`s.allocs` map is the source of truth). An operator can also use it as a **control
input** to force a client's TUN IP: edit the `ipv4` and/or `ipv6` of an owner entry in
the `kubevpn-traffic-manager` ConfigMap's `TUN_ALLOCS` key, **keeping the `version`
field** (as `kubectl edit` does; or omit it).

**Stale-echo guard (the ::1↔::3 oscillation fix).** `TUN_ALLOCS` is both the operator's
input and the server's journal output, so the reconcile must tell a genuine edit from a
stale read of the server's own write. The root cause is a **TOCTOU, not apiserver cache
staleness**: `reconcileManualOnce` reads the ConfigMap (an etcd-consistent quorum `Get`)
*before* taking `s.mu`, but snapshots `s.allocs` *after* — while a concurrent commit holds
`s.mu` across its `saveAllocs`. So the desired value (from the CM) can be from *before*
that commit's `saveAllocs` while "current" (from memory) is from *after* it → desired ≠
current → re-propose the previous committed value forever. Making the read "more
consistent" does **not** help (it already is); the CM read and the memory read straddle a
commit. The fix uses the per-owner monotonic `version` (`time.Now().UnixNano()`, bumped on
every commit/decline/expire): the reconcile **ignores any entry whose `version` is
strictly older than the in-memory allocation** — exactly such a cross-commit stale read. A
real edit keeps the current version (or omits it → 0), so it is *not* older and is
proposed. (Caveat: don't hand-set `version` to a smaller non-zero number; `kubectl edit`
preserves it.)

**Per-family & independent:** `ipv4` and `ipv6` are reconciled independently — edit
just `ipv4`, just `ipv6`, or both. Each family becomes its own proposal that the
client validates, confirms, or declines on its own (a v4+v6 edit = two confirms);
committing one family never touches the other. Leaving a field unchanged (equal to
the current value, or empty) means "no change for that family".

It uses a **dry-run (propose → client validates → confirm commits)** flow, so the
server never changes a client's committed IP before the client agrees — there is
no "commit then roll back".

```
kubectl -n <ns> edit cm kubevpn-traffic-manager   # change TUN_ALLOCS[owner].ipv4 (keep version)
   → ConfigMap informer fires (watcher.go)
   → TunConfigServer.reconcileAllocsFromConfigMap (serialized by reconcileMu):
       • stale-echo guard: skip entries whose version < in-memory allocation version
         (a lagging read of the server's own journal write — not an operator edit)
       • out-of-range / non-CIDR → Warn + ignore
       • target IP held by another live owner → REFUSE (annotate "in use"), no takeover
       • else PROPOSE: record pendingProposal[owner]={candidate,deadline} and push
         WatchTunIP{IPv4:candidate, DryRun:true}. NOTHING is committed — no rent,
         no release, no allocs change, no saveAllocs (TUN_ALLOCS keeps the edit as
         the desired value until commit).
   → Root Daemon IPWatcher (doWatchTunIP), DryRun push → handleProposal:
       • VALIDATE locally (tunIPConflicts vs sibling TUN + host interfaces, ignoring
         own current IP)
       • OK  → GetTunIP{ConfirmIP:candidate}      (confirm)
       • bad → GetTunIP{ExcludeIPs:[candidate,…]} (decline)
   → server GetTunIP = the ONLY commit point:
       • confirm  → RentSpecificIP(candidate) (fallback to a safe IP if taken since
                    propose), release owner's old IP, allocs[owner]=candidate (version
                    bumped), saveAllocs (TUN_ALLOCS=actual), syncEnvoyRuleIP, return
                    committed resp (DryRun=false) → client ChangeTunIP applies it.
       • decline  → drop proposal, keep current IP (nothing committed → no rollback),
                    bump version + saveAllocs (revert edit to actual), annotate
                    kubevpn.io/tun-allocs-rejected.
   → unconfirmed proposals expire via the lease reaper (expirePendingProposals):
     dropped + version bumped + saveAllocs reverts the edit. No IP was rented.
```

Why this is simpler: because the committed IP is never touched until the client
confirms, there is **no reservation, no defer-release, no grace-held IP, no
rollback, and scrub needs no special-casing** (a proposal holds no bitmap bit).

Client-side `ChangeTunIP` is implemented for Linux/Darwin (netlink/ioctl) and
Windows (winipcfg `DeleteIPAddress`/`AddIPAddress`). It **replaces** the address:
the old IP is removed and the new one added, both on the device's **host mask**
(`/32` for IPv4, `/128` for IPv6 — matching how the TUN device is created), and only
the family that actually changed is touched. This matters on macOS, where the add
ioctl (`SIOCAIFADDR`) is add-only — the old address must be explicitly deleted
(`SIOCDIFADDR`), otherwise the device accumulates both IPs; passing the pool mask
(`/16`) instead of `/32` would also leave the old address behind (the delete would
not match it). The client also self-heals on reconnect: `doWatchTunIP` first calls
`GetTunIP(ExcludeIPs=…)` (omitting its own current IP) so a long disconnect (lease
reaped → no push) still recovers a valid, non-conflicting IP (server prefers
`lastIPs`).

Note: the **auto recovery path** (`ReconcileDHCP`, sleep/wake) still commits a
replacement for an *already-lost* IP via `NotifyIPChange` (committed push,
`DryRun=false`); the client validates that committed push too (`applyPushedTunIP`)
and can reject → server allocates a safe IP. There is no good IP to protect there,
so dry-run is unnecessary.

**Resolved tech-debt (stale-echo guard):** both root causes are now removed. Dry-run
removed the commit-then-veto cause; the desired/actual conflation is handled by the
per-owner monotonic `version`. `TUN_ALLOCS` stays a single key (no new ConfigMap key),
but the reconcile distinguishes a genuine edit (version == current, or 0) from a stale
read of the server's own journal write (version strictly older) and ignores the latter,
so a lagging apiserver `Get` can no longer re-propose the previous committed value (the
former `::1↔::3` oscillation). Commit/decline/expire bump the version, so an applied or
rejected edit becomes strictly older and cannot be re-proposed. Trigger is constrained
to ConfigMap-edit (cluster-admin), so a client-originated CLI is out of scope. Assumes
single-writer traffic-manager (`Recreate`) and version-matched client/server (`DryRun`
understood by both); operators should keep the `version` field on edit (`kubectl edit`
does).

---

## 6. Changed Files

| File | Change type | Description |
|------|---------|------|
| **ConfigMap data-structure refactor** | | |
| `pkg/config/config.go` | modify | `KeyDHCP`+`KeyDHCP6` → `KeyTunIPPool`; `KeyClusterIPv4POOLS` → `KeyClusterCIDRs` |
| `pkg/dhcp/dhcp.go` | modify | `updateDHCPConfigMap` reads/writes the `TUN_IP_POOL` key (YAML struct with `ipv4.bitmap` + `ipv6.bitmap`) |
| `pkg/handler/connect.go` | modify | in `getCIDR`, `KeyClusterIPv4POOLS` → `KeyClusterCIDRs`; `encodeCIDRs`/`parseCachedCIDRs` use a space-separated CIDR string |
| `pkg/handler/traffmgr.go` | modify | align `createOutboundPod` ConfigMap-init keys |
| **IP-change push fix** | | |
| `pkg/xds/tun_config.go` | modify | `GetTunIP` calls `notifyWatchers` + `syncEnvoyRuleIP` on new allocation; add `syncEnvoyRuleIP`; `WatchTunIP` ticker periodic push |
| **Proxy mode unification** | | |
| `pkg/xds/cache.go` | modify | `Virtual.To()` generates a UDP listener + `udp_proxy` filter for UDP ports; add `toUDPListener` |
| `pkg/inject/injector.go` | modify | `NewInjector` drops the `vpnInjector` branch; VPN-only goes through `meshInjector` |
| `pkg/inject/vpn.go` | **delete** | `vpnInjector` no longer needed |
| `pkg/inject/container.go` | modify | delete `AddVPNContainer` (unify on `AddVPNAndEnvoyContainer`); drop the `LocalTunIPv4/v6` env vars |
| `pkg/inject/envoy.yaml` | modify | add base UDP listener config to the bootstrap template |
| `pkg/core/protocol_registry.go` | simplify | delete `watchTunIPChanges` + `pollTunIP` + `applyTunIPChange` (envoy handles routing) |
| **Manual TUN_ALLOCS IP override (dry-run, §5b)** | | |
| `pkg/daemon/rpc/daemon.proto` | modify | add `TunIPResponse.DryRun` + `TunIPRequest.ConfirmIP` (regen `*.pb.go`) |
| `pkg/xds/tun_config.go` | modify | `ReconcileAllocsFromConfigMap` (exported) + per-family `proposeManualChange`/`proposeIPChange`; `GetTunIP` confirm/decline + `commitFamilyLocked`; `pendingProposal`; `expirePendingProposals`; `WatcherCount` |
| `pkg/xds/controlplane.go` | modify | wire `ReconcileAllocsFromConfigMap` as an informer onDHCPChange callback |
| `pkg/dhcp/dhcp.go` | modify | add `RentSpecificIP` + `InRange` (per-family specific allocation) |
| `pkg/handler/network.go` | modify | `doWatchTunIP` per-family `handleProposal` (validate→confirm/decline) + reconnect self-heal; `ChangeTunIP` replaces address on host masks (`/32`,`/128`), only the changed family |
| `pkg/tun/ip_windows.go` | modify | implement `changeIP` (winipcfg Delete/AddIPAddress) |
| `pkg/tun/ip_darwin.go`, `pkg/tun/tun_darwin.go` | modify | `changeIP` deletes the old address first; add `removeInterfaceAddress`/`delInet4Address`/`delInet6Address` (`SIOCDIFADDR`) so macOS doesn't keep both IPs |

The §5b manual-IP override adds **two optional proto fields** (`DryRun`, `ConfirmIP`); everything else still
reuses the existing ENVOY_CONFIG → Watcher → Processor → xDS push path and the WatchTunIP push.

---

## 7. Design Comparison

| Dimension | Before | After |
|------|-------|-------|
| Injection strategies | 3 (vpn / mesh / fargate) | 2 (mesh / fargate); VPN-only = a mesh with empty headers |
| sidecar containers | VPN-only: VPN only / Mesh: VPN + envoy | unified: VPN + envoy |
| sidecar DNAT | VPN-only: `DNAT → ${IP}` (hard-coded) / Mesh: `DNAT → :15006` | unified: `DNAT → :15006` (fixed port) |
| TCP routing | VPN-only: iptables direct forward / Mesh: envoy | unified: envoy (header routing + origin_cluster default back to origin) |
| UDP routing | VPN-only: iptables forward / Mesh: unsupported | unified: envoy udp_proxy (cluster → endpoint) |
| IP hot-update | VPN-only: ❌ / Mesh: xDS push | unified: xDS push |
| ENVOY_CONFIG update | written once at inject time | `syncEnvoyRuleIP` auto-updates on IP change |
| client TUN IP | WatchTunIP (broken push path) | WatchTunIP (fixed NotifyIPChange + ticker fallback) |
| code volume | vpn.go + AddVPNContainer + watchTunIPChanges | all deleted, unified on the mesh path |
| new server code | — | `syncEnvoyRuleIP` + `toUDPListener` |
| extra resources | — | VPN-only gains one more envoy container (~200MB) |

---

## 8. Compatibility

A fresh design, incompatible with old sidecars. After upgrading, run `kubevpn reset` to clean up old
sidecars + re-run `kubevpn proxy` to inject the new sidecar.

---

## 9. Verification

```bash
# build
go build ./...
go vet ./pkg/...

# unit tests
go test ./pkg/xds/... -v
go test ./pkg/inject/... -v

# integration test scenarios:

# 1. VPN-only (no headers)
kubevpn proxy deploy/web
# → verify: envoy sidecar injected, TCP+UDP full hijack, traffic OK

# 2. Mesh (with headers)
kubevpn proxy deploy/web --headers version=v1
# → verify: header routing works, non-matching requests go back to the origin service

# 3. Sleep/wake simulation
# → delete the TUN_ALLOCS entry in the traffic manager pod (simulate lease expiry)
# → wait for the health check to trigger GetTunIP (within 30s)
# → verify:
#    local TUN IP updated (ip addr show utun0)
#    Rule.LocalTunIPv4 in ConfigMap ENVOY_CONFIG updated
#    envoy routing hot-updated (traffic recovered)

# 4. UDP test
# → send data to a proxied workload's UDP port
# → verify envoy udp_proxy forwards it correctly to the user's machine
```
