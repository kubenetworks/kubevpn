# DHCP IP Allocation Design

## 1. Overview

KubeVPN uses a custom DHCP mechanism to assign a unique TUN device IP address to each VPN connection. Instead of relying on a traditional DHCP server, it uses a **Kubernetes ConfigMap as distributed storage** combined with a **bitmap allocator** to achieve stateless, conflict-free IP allocation.

### Design Goals

- Multi-user concurrency safety (via K8s optimistic locking with `RetryOnConflict`)
- No standalone DHCP server process required
- IPv4 + IPv6 dual-stack support
- Automatic IP reclamation (lease expiration mechanism)
- IP allocation state recoverable after cluster restart

## 2. IP Address Pool

| Protocol | CIDR | Address Count | Purpose |
|----------|------|---------------|---------|
| IPv4 | `198.18.0.0/16` | 65,534 | TUN device IP (IANA reserved for benchmarking) |
| IPv6 | `2001:2::/64` | ~2^64 | TUN device IPv6 address (IANA reserved for benchmarking) |
| Docker IPv4 | `198.19.0.0/16` | 65,534 | kubevpn run Docker bridge network |

`198.18.0.0/15` is reserved by IANA for network benchmarking (RFC 2544) and does not conflict with real networks. KubeVPN splits it into two /16 subnets: `198.18.0.0/16` for TUN and `198.19.0.0/16` for Docker.

## 3. Architecture

```
┌──────────────────────────────────────────────────────────┐
│                    Traffic Manager Pod                     │
│                                                           │
│  TunConfigServer (:9002 gRPC)                             │
│  ├── allocs map[ownerID]*tunAllocation  ← in-memory cache │
│  ├── GetTunIP(ownerID, excludeIPs)      ← allocate/renew  │
│  ├── WatchTunIP(ownerID)                ← push IP changes │
│  ├── LeaseReaper (30s tick)             ← reclaim expired  │
│  └── dhcp.Manager                       ← bitmap allocator │
│       ├── RentIP / RentIPExcluding                        │
│       ├── ReleaseIP                                       │
│       └── ForEach                                         │
│                                                           │
│  ConfigMap: kubevpn-traffic-manager                        │
│  ├── TUN_IP_POOL = yaml{ipv4{cidr,bitmap},               │
│  │                       ipv6{cidr,bitmap}} ← dual-stack  │
│  ├── TUN_ALLOCS   = yaml(ownerID→IP)     ← persisted map  │
│  ├── CLUSTER_CIDRS = "cidr cidr ..."     ← cluster CIDRs  │
│  └── ENVOY_CONFIG  = yaml([]*Virtual)    ← envoy routes   │
└──────────────────────────────────────────────────────────┘
         ↑ port-forward
┌─────────────────────┐
│  Root Daemon         │
│  NetworkManager      │
│  └── rentIP()        │
│       → GetTunIP()   │
└─────────────────────┘
```

## 4. Core Components

### 4.1 dhcp.Manager (Low-Level Allocator)

`pkg/dhcp/dhcp.go` — Wraps the cilium/ipam bitmap allocator.

**Responsibility:** Perform atomic read-modify-write operations on the ConfigMap to allocate/release IPs.

```
RentIPExcluding(ctx, excludeIPs):
  1. GET ConfigMap
  2. parse TUN_IP_POOL (YAML) → {ipv4,ipv6}.bitmap (base64) → ipallocator.Range
  3. AllocateNext() in a loop until an IP not in excludeIPs is found
  4. Snapshot() → base64 → re-marshal TUN_IP_POOL → UPDATE ConfigMap
  (RetryOnConflict automatically retries on conflict)
```

> The IPv4 and IPv6 bitmaps are stored together under the single `TUN_IP_POOL`
> key as a YAML struct (`ipv4.cidr`/`ipv4.bitmap` + `ipv6.cidr`/`ipv6.bitmap`).
> This replaced the former separate `DHCP` (v4) and `DHCP6` (v6) keys.
> `RentIP`/`ReleaseIP` operate on both stacks in a single read-modify-write.

**Key Methods:**

| Method | Description |
|--------|-------------|
| `RentIP(ctx)` | Allocate the next available IPv4 + IPv6 |
| `RentIPExcluding(ctx, excludeIPs)` | Allocate an IP, skipping addresses in the excludeIPs list |
| `ReleaseIP(ctx, v4, v6)` | Release specified IPs back to the pool |
| `ForEach(ctx, fnv4, fnv6)` | Iterate over all allocated IPs |
| `InitDHCP(ctx)` | Ensure the ConfigMap exists |

**Bitmap Allocator (cilium/ipam):**

Uses `ContiguousAllocationMap` — sequentially scans the bitmap starting from offset 0 to find the first free bit. Properties:
- Deterministic allocation (always returns the lowest available IP)
- `Snapshot()` / `Restore()` support serialization to `[]byte`
- Allocation and release have O(n) time complexity, where n is the address pool size

**Concurrency Safety:**

Each key in the ConfigMap is updated independently via JSON Patch (`JSONPatchType`), so writes to `TUN_IP_POOL` do not conflict with writes to `TUN_ALLOCS` or `ENVOY_CONFIG`. The DHCP bitmap update still requires a GET (to parse the current bitmap) followed by a Patch; `RetryOnConflict` retries on conflict with other DHCP operations.

**Error Handling:**

All `ReleaseIP` calls check and log errors — silent failures could cause permanent IP leaks (bits stuck in the bitmap).

### 4.2 TunConfigServer (Control Plane)

`pkg/xds/tun_config.go` — gRPC service running in the Traffic Manager Pod.

**Responsibility:** Manage ownerID → IP mappings, lease renewal, and IP change notifications.

**In-Memory State:**
```go
allocs map[string]*tunAllocation  // ownerID → {IPv4, IPv6, Version, LastRenew, Hostname}
```

**GetTunIP Flow:**

The entire method runs under `s.mu.Lock()` to prevent concurrent allocations from obtaining the same IP. `RentIPExcluding` is called within the lock (it's a fast ConfigMap operation).

```
GetTunIP(ownerID, excludeIPs):          // holds s.mu.Lock for entire method
  ├── exists in allocs and no conflict → return existing IP (renew LastRenew)
  ├── exists in allocs but conflicts with excludeIPs →
  │     1. dhcp.RentIPExcluding(excludeIPs) to allocate a new IP
  │     2. dhcp.ReleaseIP(old IP)
  │     3. saveAllocs (synchronous, returns error)
  │     4. notifyWatchers + syncEnvoyRuleIP
  │     5. return new IP
  └── not in allocs →
        1. dhcp.RentIPExcluding(excludeIPs) to allocate a new IP
        2. saveAllocs (synchronous, returns error)
        3. notifyWatchers + syncEnvoyRuleIP
        4. return new IP
```

**Persistence (allocs → ConfigMap):**

The allocation mapping is stored as YAML in the `TUN_ALLOCS` key of the ConfigMap.

**Single-User Example:**
```yaml
{
  "a1b2c3d4e5f6": {
    "ipv4": "198.18.0.5/16",
    "ipv6": "2001:2::5/64",
    "version": 1717900000000000000,
    "lastRenew": 1717900000,
    "hostname": "dev-laptop-01"
  }
}
```

**Multi-User + Multi-Cluster Example:**

Three users simultaneously connected to the same cluster namespace `default`, sharing the same ConfigMap:
```yaml
{
  "a1b2c3d4e5f6": {
    "ipv4": "198.18.0.5/16",
    "ipv6": "2001:2::5/64",
    "version": 1717900000000000000,
    "lastRenew": 1717900120
  },
  "f6e5d4c3b2a1": {
    "ipv4": "198.18.0.6/16",
    "ipv6": "2001:2::6/64",
    "version": 1717900100000000000,
    "lastRenew": 1717900200
  },
  "112233445566": {
    "ipv4": "198.18.0.7/16",
    "ipv6": "2001:2::7/64",
    "version": 1717900200000000000,
    "lastRenew": 1717900300
  }
}
```

Field descriptions:
- `ownerID` (key) — Unique identifier for the connection, first 12 characters of a UUID, generated on each connect
- `ipv4` / `ipv6` — Allocated TUN IP with CIDR mask
- `version` — Monotonically increasing version number (`time.Now().UnixNano()`), used for WatchTunIP change detection
- `lastRenew` — Last renewal time (Unix seconds), used by LeaseReaper to determine expiration
- `hostname` — Client machine name reported by the data-plane daemon (`os.Hostname()`), recorded purely for debugging so an operator can map an ownerID to a physical machine. Omitted (`omitempty`) when the client sends none

**Expiration Scenario:** When user `112233445566` disconnects for more than 5 minutes, LeaseReaper deletes that entry and calls `dhcp.ReleaseIP` to release `198.18.0.7`, clearing the corresponding bit in the bitmap so the IP can be reused by a new user.

On startup, `loadAllocs` restores from ConfigMap, and expired allocations are released immediately.

### 4.3 Lease Mechanism

**Parameters:**
- `LeaseDuration = 5 minutes` — Validity period for IP allocations
- `LeaseReaper interval = 30 seconds` — Frequency of checking for expired IPs
- `WatchTunIP renewal = LeaseDuration / 3` ≈ 100 seconds — Implicit renewal interval via stream

**Renewal Methods:**

| Source | Renewal Trigger |
|--------|----------------|
| `GetTunIP` call | Each call refreshes `LastRenew` |
| `WatchTunIP` stream | Background ticker auto-renews every ~100s |

**Expiration Reclamation:**

```
LeaseReaper (every 30s):
  collect expired ownerIDs (under read lock)
  for each expired ownerID:
    re-acquire write lock
    double-check: if alloc still exists AND still expired:  // prevents deleting a reconnected client
      delete(allocs, ownerID)
      dhcp.ReleaseIP(alloc.IPv4, alloc.IPv6)  // errors logged, not ignored
  saveAllocs()
```

**Design Rationale:** No explicit IP release is required. After a client disconnects, the lease naturally expires and LeaseReaper reclaims it. This avoids IP leaks caused by incomplete cleanup during disconnection.

## 5. ExcludeIPs Conflict Avoidance

### Problem

TUN IPs come from `198.18.0.0/16`, which may conflict with IP addresses on local network interfaces. The bitmap allocator has no awareness of the local network environment.

### Solution

During `rentIP`, the client collects all local interface IPs and passes them as `ExcludeIPs` to `GetTunIP`. The server skips these IPs during DHCP allocation.

```
Root Daemon (startTUN → rentIP):
  1. collectLocalIPs() → ["192.168.1.100", "198.18.0.1", ...]
  2. GetTunIP(ownerID, excludeIPs=collectLocalIPs())
  3. Server: dhcp.RentIPExcluding(excludeIPs)
  4. If the allocated IP happens to conflict (very low probability race condition) → isLocalIPConflict → retry up to 15 times
```

**Why not release then re-allocate?** The bitmap uses `contiguousScanStrategy`, which scans sequentially from offset 0. After releasing an IP and re-allocating, the same IP would always be returned, causing an infinite loop. `ExcludeIPs` skips conflicting IPs within a single DHCP transaction, resolving the issue in one step.

## 6. Data Flow (Complete IP Allocation Path)

```
kubevpn connect -n default
  │
  ├─ User Daemon:
  │   CreateOutboundPod → ensure traffic manager pod is running
  │   → req.OwnerID = uuid[:12]
  │   → cli.Connect(req) → Root Daemon
  │
  ├─ Root Daemon:
  │   DoConnect → NetworkManager.Start()
  │     → portForward(:10801, :10802, :9002)
  │     → startTUN:
  │         1. collectLocalIPs()
  │         2. gRPC GetTunIP(ownerID, excludeIPs) → TunConfigServer
  │         3. TunConfigServer:
  │            ├─ dhcp.RentIPExcluding(excludeIPs) → bitmap allocation
  │            ├─ store in allocs[ownerID]
  │            └─ return 198.18.0.5/16
  │         4. isLocalIPConflict? → retry or use
  │         5. tun.Listener(tunConfig{Addr: "198.18.0.5/32"})
  │     → StartIPWatcher → WatchTunIP(ownerID)
  │
  ├─ IP Change:
  │   TunConfigServer → ReconcileDHCP / external trigger
  │     → NotifyIPChange → WatchTunIP stream push
  │     → Root Daemon: ChangeTunIP(newIPv4, newIPv6)
  │
  └─ Disconnection:
      → Lease expires (5 min) → LeaseReaper reclaims
      → corresponding bit cleared in bitmap → IP available for reuse by new users
```

## 7. ConfigMap Data Format

ConfigMap name: `kubevpn-traffic-manager`

| Key | Format | Content |
|-----|--------|---------|
| `TUN_IP_POOL` | YAML `{ipv4{cidr,bitmap}, ipv6{cidr,bitmap}}` | Dual-stack TUN IP allocation bitmaps (base64) — merges the former `DHCP` + `DHCP6` |
| `TUN_ALLOCS` | YAML | `map[ownerID]{ipv4, ipv6, version, lastRenew, hostname}` (hostname is debug-only, omitempty) |
| `ENVOY_CONFIG` | YAML | Envoy routing rules `[]*Virtual` |
| `CLUSTER_CIDRS` | Space-separated text | Cluster Pod/Service CIDRs (IPv4 + IPv6) — renamed from `IPv4_POOLS` |

Consistency between bitmap and allocs is guaranteed by K8s `resourceVersion` optimistic locking.

> **Key rename history:** `DHCP` + `DHCP6` → `TUN_IP_POOL` (merged dual-stack
> struct); `IPv4_POOLS` → `CLUSTER_CIDRS` (the old name was inaccurate — it
> always held both IPv4 and IPv6 CIDRs). Constants live in `pkg/config/config.go`
> (`KeyTunIPPool`, `KeyClusterCIDRs`).

## 8. Failure Scenarios

| Scenario | Behavior |
|----------|----------|
| Client disconnects unexpectedly | Lease expires after 5 minutes, LeaseReaper auto-reclaims |
| Traffic Manager Pod restarts | `loadAllocs` restores in-memory state from ConfigMap, bitmap is not lost |
| ConfigMap manually deleted | `InitDHCP` recreates an empty ConfigMap, all IPs are re-allocated |
| Two users allocate simultaneously | `RetryOnConflict` optimistic locking ensures atomicity, the latter retries |
| Allocated IP conflicts with local interface | `ExcludeIPs` mechanism skips conflicting IPs |
| IP pool exhausted | `AllocateNext` returns an error, connection fails |
| WatchTunIP long-lived connection | Implicit renewal every ~100s prevents LeaseReaper from incorrectly reclaiming |

## 9. Related Files

| File | Purpose |
|------|---------|
| `pkg/dhcp/dhcp.go` | DHCP Manager — bitmap allocator wrapper |
| `pkg/xds/tun_config.go` | TunConfigServer — IP allocation control plane |
| `pkg/config/config.go` | CIDR, ConfigMap key name constants, etc. |
| `pkg/handler/network.go` | NetworkManager.rentIP — client-side IP acquisition |
| `pkg/daemon/rpc/daemon.proto` | TunIPRequest/TunIPResponse proto definitions |
