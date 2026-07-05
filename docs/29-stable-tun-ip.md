# Stable / Fixed TUN IP Allocation Design

## 1. Background & Problem

KubeVPN allocates a TUN device IP per VPN connection (pool `198.18.0.0/16`, see
`docs/03-dhcp-ip-allocation.md`). Allocation is owned by the in-cluster traffic-manager's
`TunConfigServer`; leases are keyed by **OwnerID** in the ConfigMap's `TUN_ALLOCS` key, and the
underlying bitmap allocation uses `cilium/ipam`.

**Problem: the same client gets an unstable TUN IP on every reconnect.**

Root cause: OwnerID is randomly generated on every connect.

```go
// pkg/daemon/action/connect_elevate.go:31 (inside the user daemon)
ownerID := req.OwnerID
if ownerID == "" {
    ownerID = uuid.New().String()[:12] // ← a fresh random value on every connect
}
```

This produces two kinds of instability:

| Scenario | Current behavior | Consequence |
|------|------|------|
| Reconnect within the lease (< 5min) | The old OwnerID's lease still holds the old IP; the new OwnerID gets **another** IP | IP changes; the old IP is wasted until it expires |
| Reconnect after lease reclaim (> 5min) | The bitmap is scanned from 0 for the "next free" slot, which only **coincidentally** may match | IP is unpredictable |

## 2. Goals & Scope

- **Goal**: best-effort return of the **same** TUN IP to the same client after a reconnect.
- **Granularity**: identity is per **(machine + OS user)** — stable and unique for the same machine and same OS user.
- **Nature**: best-effort, **not** a hard permanent reservation — if offline long enough and the IP has been taken by someone else, it cleanly falls back to a new IP.
- **Out of scope**:
  - Permanent reservation / static allocation (never reclaimed while offline, never taken by others).
  - Per-"person" pinning across machines (based on kubeconfig user/cert).

The design has two layers, applied together:

- **Part A — stable OwnerID**: handles reconnect within the lease (the high-frequency case), and eliminates "waste one IP per reconnect".
- **Part B — preferred-IP hint**: handles reconnect after lease reclaim (long disconnect).

## 3. Current Infrastructure (reusable building blocks)

- Lease layer `pkg/xds/tun_config.go`: `GetTunIP` already supports
  - returning the same IP when the same OwnerID renews (`s.allocs[ownerID]` hit returns it);
  - reallocation on `ExcludeIPs` conflict.
- Bitmap layer `pkg/dhcp/dhcp.go`: `RentIPExcluding` uses `AllocateNext` (sequential allocation).
- The underlying `cilium/ipam` offers both **specific-IP allocation** and sequential allocation:
  ```go
  // vendor/github.com/cilium/ipam/service/ipallocator/allocator.go
  func (r *Range) Allocate(ip net.IP) error   // specific IP; errors if taken
  func (r *Range) AllocateNext() (net.IP, error)
  ```
- The client allocation entry point `rentIP` in `pkg/handler/network.go` (runs in the **root daemon**)
  calls `GetTunIP` via `TunIPRequest{OwnerID, Namespace, ExcludeIPs}`.
- Persistence directory `~/.kubevpn` (`homePath` in `pkg/config/const.go`); the project already depends on
  `github.com/google/uuid`.

## 4. Design

### 4.1 Part A: Stable OwnerID (persisted UUID, per machine + user)

Add `config.GetClientID()`:

```
First call:   generate a UUID → write to ~/.kubevpn/client_id (if the file is absent)
Later calls:  read and reuse
Return value: first 12 chars, matching the existing OwnerID format
```

In `connect_elevate.go`, replace the random generation with:

```go
if ownerID == "" {
    ownerID = config.GetClientID() // stable, persisted
}
```

This runs in the **user daemon**, where `~/.kubevpn` is the caller's (OS user's) home directory, so the
OwnerID automatically satisfies:

- Same machine, same OS user → stable and unique, unchanged across reconnects and daemon restarts;
- Different OS user → different home directory → different OwnerID.

**Effect of Part A alone**: a reconnect within the lease (5min) hits `s.allocs[ownerID]` and returns the
same IP directly; no longer consumes a new IP per reconnect.

### 4.2 Part B: Server-side "last IP" memory (stickiness after a long disconnect)

After a lease is reclaimed, that OwnerID's record in `TUN_ALLOCS` is gone. To return the old IP on
reconnect, something must remember "which IP this OwnerID last used".

**Implementation note**: the original design had the client send its last IP as `PreferredIPs` over gRPC,
which would require a new field on `TunIPRequest` and re-running `make gen` for protobuf. Since the build
environment has no `protoc` and the repo convention is to not hand-edit `*.pb.go`, we use an **equivalent
server-side implementation**: now that Part A makes the OwnerID stable, the traffic-manager itself
remembers "the IP last allocated to this OwnerID" by OwnerID — no new protocol field and no client-side
persistence needed. Cross-cluster local uniqueness is still guaranteed by the `ExcludeIPs` the client
already sends (Fix 3).

#### 4.2.1 Server-side `lastIPs` memory

`TunConfigServer` in `pkg/xds/tun_config.go` gains an in-memory map:

```go
lastIPs map[string]lastIPRecord // ownerID → last held {v4, v6}
```

- **When written**: in `reapExpiredLeases`, before reclaiming an OwnerID's lease and calling
  `delete(allocs, ownerID)`, first set `lastIPs[ownerID] = {alloc.IPv4, alloc.IPv6}`.
- **Properties**: in-memory only (lost on traffic-manager restart, see §7); keyed by OwnerID, which Part A
  keeps stable and unique per user, so the entry count is naturally bounded by the real client count
  (no longer unbounded growth as with random UUIDs).

#### 4.2.2 Bitmap layer: specific-IP support

`pkg/dhcp/dhcp.go`: refactor `RentIPExcluding` into an internal `rentIP` sharing `makeShouldSkip` /
`allocateOne`, and add:

```go
// Try ipallocator.Allocate(specific IP) first; fall back to AllocateNext if taken/unavailable/skipped.
func (m *Manager) RentIPPreferring(ctx, prefV4, prefV6 net.IP, exclude []net.IP) (*net.IPNet, *net.IPNet, error)
```

#### 4.2.3 GetTunIP: prefer reuse

`GetTunIP` in `pkg/xds/tun_config.go`, **only on the "new allocation" path** (the OwnerID has no
active lease), calls `allocateForOwner`:

```
if lastIPs[ownerID] exists AND that IP is not in ExcludeIPs
        → RentIPPreferring(lastIP, exclude)   // prefer reusing the old IP
else    → RentIPExcluding(exclude)             // sequential allocation (unchanged)
```

The "renew existing lease" and "reallocate on ExcludeIPs conflict" paths are unchanged. All Fix 1/2
invariants (single writer, rollback on persistence failure, orphan-bit cleanup) are preserved.

### 4.3 Data Flow

```
Reconnect (same client)
  user daemon: OwnerID = GetClientID()  ──(ConnectRequest.OwnerID)──▶ root daemon
  root daemon rentIP: GetTunIP{OwnerID, ExcludeIPs} ─────▶ traffic-manager
        ├─ active lease       → return original IP (Part A)
        └─ no active lease, allocateForOwner:
              lastIPs[OwnerID] exists and not in ExcludeIPs → RentIPPreferring(old IP)
                    old IP free → reuse (Part B)
                    otherwise   → AllocateNext fallback
              otherwise                                     → RentIPExcluding (sequential)
```

## 5. Multi-cluster / Multi-user / Multi-sidecar Correctness

| Dimension | Analysis | Conclusion |
|------|------|------|
| **Multiple users, same cluster** | OwnerID differs by OS user home directory; `lastIPs` reuse only hits when the IP is **free**, otherwise `Allocate` fails and falls back | No two users share an IP |
| **One client, multiple clusters** | Each cluster has its own ConfigMap/allocs; the same OwnerID is fine, but the local machine needs **non-overlapping** TUN IPs across clusters | See next row |
| **Cross-cluster local uniqueness** | The client's `ExcludeIPs` includes the TUN IPs held by sibling connections (Fix 3, `buildExcludeIPs`); the server checks `ExcludeIPs` before reusing `lastIPs`, skipping on a hit | Locally unique across clusters; reuse yields |
| **Multiple sidecars** | A sidecar's OwnerID is its podName — a disjoint key space from client UUIDs | No conflict |
| **One client, multiple connections, same cluster** | The user daemon dedups by ConnectionID (manager-namespace UID); at most one connection per cluster | The same OwnerID never has two leases in one cluster |

## 6. Relationship to Existing Mechanisms

- **Lease / reaper** (`docs/03`): unchanged. `lastIPs` only affects "how to pick an IP when there is no
  active lease"; it does not change lease duration or reclaim.
- **Rolling-upgrade safety Fix 1**, **bitmap↔allocs leak prevention Fix 2**, **envoy rule IP sync Fix 4**:
  all unaffected, still in effect.
- **Cross-cluster local IP exclusion Fix 3**: `lastIPs` reuse is constrained by the client-sent
  `ExcludeIPs`; local uniqueness takes priority over "reuse the same IP".
- **Sleep/wake `docs/28`**: stable OwnerID + `lastIPs` raise the probability of getting the same IP back
  after wake; the VPN-only sidecar's hard-coded iptables DNAT hot-update is still a separate legacy item,
  out of scope here.

## 7. Failure & Fallback (edge cases)

| Scenario | Behavior |
|------|------|
| `~/.kubevpn/client_id` not writable | Fall back to a random UUID for this run (degrades to current behavior, no functional impact) |
| Old IP already taken by someone else | `Allocate` fails → `AllocateNext` falls back to a new IP |
| Old IP conflicts with another local cluster | It is in `ExcludeIPs` → skip reuse → fall back |
| traffic-manager restart | In-memory `lastIPs` is lost → degrades to Part A (active leases still stable; a reclaimed, expired connection may change IP on its first reconnect, then is stable again) |

## 8. Interfaces & File List

| File | Change |
|------|------|
| `pkg/config/clientid.go` (new) | `GetClientID()` (persist a stable client UUID) |
| `pkg/config/const.go` | reuse `homePath` |
| `pkg/daemon/action/connect_elevate.go` | use `GetClientID()` instead of a random UUID |
| `pkg/dhcp/dhcp.go` | refactor `rentIP`/`makeShouldSkip`/`allocateOne`, add `RentIPPreferring` |
| `pkg/xds/tun_config.go` | `lastIPs` memory + `allocateForOwner`; `GetTunIP` new-allocation path prefers reuse |

> Note: the originally planned `daemon.proto` new field and `pkg/handler/network.go` client change were
> replaced by the server-side `lastIPs` approach (see §4.2) due to the lack of a `protoc` environment, and
> are no longer involved.

## 9. Testing & Verification

Integration tests (`fake.NewSimpleClientset` + `controlplane`'s `newTestServer`):

- Same OwnerID reconnecting after reclaim → reuses the old IP, even if a lower-numbered free IP exists
  (`TestGetTunIP_StickyReconnectPrefersRememberedIP`).
- Old IP already taken by someone else → falls back to another free IP, no cross-wiring
  (`TestGetTunIP_StickyFallsBackWhenRememberedIPTaken`).
- Old IP appears in `ExcludeIPs` → yields and falls back (`TestGetTunIP_StickyYieldsToExcludeIPs`, Fix 3 priority).
- Same OwnerID renewing → same IP (already covered by `TestGetTunIP_RenewReturnsSameIP`).
- `config.GetClientID()` is stable and persisted across calls (`TestGetClientID_StableAndPersisted`).

General: `go build ./...` (linux/darwin/windows), `go vet ./pkg/...`,
`go test ./pkg/...` (known pre-existing failures: `TestPing` environment limitation, occasional
`TestTUN_FullDataPath_HTTPRequest`).

End-to-end (`/data/.kube/config`, switch to the remote context): connect and record the IP → disconnect →
reconnect within 5min (Part A, same IP) and after >5min (Part B, same IP if free); connect to two clusters
concurrently to verify the local IPs are still distinct.
