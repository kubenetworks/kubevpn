# Server-Side Route & Service Discovery

## 1. Problem

Every client connection used to run its **own** pod and service informers to keep the
local route table and `/etc/hosts` current (`AddRouteDynamic` in `pkg/handler/network.go`):

- a **cluster-wide pod list-watch** (when the user's kubeconfig allowed it), whose only
  effect was adding per-pod `/32` routes — routes that are already covered by the cluster
  **pod CIDR** installed at connect time (`startTUN`), and skipped by `AddRoute` when
  already routed. It was a redundant catch-all that still paid the full cost of a
  cluster-wide watch (and re-listed on every watch error).
- a service informer feeding both routing and DNS/hosts.

With N connections this is N cluster-wide watches. On a resource-starved apiserver (e.g.
the macOS-CI nested-VM minikube, see [43-macos-ci-apiserver-flakiness.md](43-macos-ci-apiserver-flakiness.md))
these watches — and their aggressive re-list on error — amplify a transient stall into a
sustained one.

## 2. Design

Move discovery **into the traffic manager**, pushed to clients over the existing
`TunConfigService` gRPC stream (port 9002, the same channel used for TUN IP hot-update,
[09-tun-ip-hot-update.md](09-tun-ip-hot-update.md)).

```
rpc WatchNamespaceRoutes(NamespaceRoutesRequest) returns (stream NamespaceRoutesResponse)
```

- **Per-namespace, not cluster-wide.** The manager runs one pod + one service informer
  **scoped to the workload namespace** the client subscribes with, created lazily on first
  subscribe and garbage-collected when the last subscriber leaves (`pkg/xds/route_discovery.go`).
- **Low memory.** Each informer uses a `TransformFunc` that keeps only the fields needed
  (`Name`/`Namespace` for the cache key, pod `HostNetwork` + IPs, service
  `ClusterIP(s)`/`ExternalName`) and drops the rest. Only `Name`+`Namespace` are required
  by the informer machinery — the reflector tracks resource version from the List/Watch
  response metadata and raw watch events, not from cached objects.
- **CIDR aggregation.** Pod IPs are aggregated to a fixed prefix (IPv4 `/24`, IPv6 `/64`)
  before being pushed, collapsing thousands of per-pod `/32` into a handful of prefixes
  (≈ one per node podCIDR the namespace spans). Aggregation is **fixed-granularity only**
  — never a minimal-cover of arbitrary IPs — so it can never widen into non-cluster space.
- **Snapshot + delta, gzip.** The first frame is a full snapshot (`Snapshot=true`);
  subsequent frames are deltas diffed against the last snapshot, debounced (~300ms) to
  coalesce rollout churn. The stream uses gzip. There is **no lease heartbeat** — routes
  are not leased; a dropped push self-heals via reconnect (the server resends a snapshot)
  and `AddRoute` is idempotent.

### Client

`NetworkManager.StartRouteWatcher` subscribes for the workload namespace and applies frames:

- `AddedPodCIDRs` → `AddRouteCIDR` (whole prefixes, via the same TUN route path as the
  cluster CIDRs), on top of the cluster CIDR routes already installed at connect.
- `UpsertedServices` / `RemovedServiceKeys` → maintained in a `services` map fed to DNS via
  `dns.Config.UpdateServices`, **and** each upserted service's ClusterIPs are added to the
  route table via `AddRoute`. The whole service CIDR is normally routed at connect, but that
  detection is best-effort and can miss or exclude the range (managed clusters hide the
  component flags, the invalid-ClusterIP error message may not match, or the range is dropped
  by the API-server-IP filter — [32-cidr-detection.md](32-cidr-detection.md) §5). Without a
  per-service route the name still resolves (hosts entry) but the connection has no path, so
  the ClusterIPs are routed explicitly. `AddRoute` skips API server IPs and already-routed IPs
  and is idempotent, so this is cheap and a no-op when the service CIDR already covers the IP.
  This restores the per-service routing the pre-refactor client informer provided, now scoped
  to the subscribed namespace.

The client no longer runs any pod/service informer. DNS (`pkg/dns`) dropped its
`SvcInformer` dependency: hosts are written push-driven by `UpdateServices` (add-only,
matching prior behavior) and macOS resolver files are refreshed via `applyResolvers`.

### Degradation (no client fallback)

`kubevpn` upgrades an older traffic manager to match the client before connecting
(`UpgradeDeploy`), so `WatchNamespaceRoutes` is normally available. If it is not
(`codes.Unimplemented` on an old manager, or `Enabled=false` when the manager lacks RBAC),
the client **degrades to CIDR-only routing** — the cluster pod/service CIDRs already cover
routing; only per-pod-IP catch-all and short-name hosts entries are absent. There is **no
client-side fallback informer**.

## 3. RBAC

Server-side discovery needs the manager ServiceAccount to `list,watch` pods and services in
the workload namespace. This is granted by a **namespaced** Role + RoleBinding named
`kubevpn-traffic-manager-route` (distinct from the manager's own Role so it never clobbers
it when workload ns == manager ns), created idempotently and **best-effort** on every
`CreateOutboundPod` (`ensureRouteRBAC`). `list`/`watch` cannot be `resourceNames`-scoped, so
the rule is namespaced without a name filter — the least-privilege way to grant it without
any **cluster-scoped** RBAC object. (Route discovery itself introduces no cluster-scoped
RBAC. Server-side sidecar injection, added later, does use one ClusterRole for a *central*
manager in the kubevpn namespace — see [17-sidecar-injection.md](17-sidecar-injection.md)
§7 and [24-traffic-manager-deployment.md](24-traffic-manager-deployment.md); per-namespace
managers stay namespaced.) If the connecting user cannot
create this RBAC, discovery degrades (see above). Cleanup removes the `-route` RBAC in the
manager namespace; in central mode a harmless dangling copy may remain in the workload
namespace (its subject SA is deleted on uninstall).

## 3b. Zombie rule cleanup (abandonment TTL)

A client that vanishes without a clean Leave (crash / SIGKILL / powered off) leaves its
envoy proxy rule in ENVOY_CONFIG. The lease reaper cleans these up, but **only after a long
`abandonmentTTL`** measured from when the lease was reaped — never on the lease reap itself.
This distinction matters: a lease expiring does **not** mean the client is gone. A sleeping
laptop's `WatchTunIP` stream drops and its lease is reaped, but on wake it re-allocates and
`syncEnvoyRuleIP` re-points the still-present rule (see [09-tun-ip-hot-update.md](09-tun-ip-hot-update.md)
and [28-sleep-wake-ip-update.md](28-sleep-wake-ip-update.md)). Removing the rule on reap would
break that; so instead:

- On reap, the owner's reap time is recorded (`reapedAt`); `GetTunIP` clears it (the owner is
  back). The abandonment pass removes an owner's rules only when `reapedAt` is older than
  `abandonmentTTL` **and** it has no live alloc and no active watcher.
- `abandonmentTTL` defaults to **24h** (deliberately far longer than the 5-min lease, so even
  overnight sleep is unaffected) and is overridable via `KUBEVPN_PROXY_ABANDON_TTL` (a Go
  duration). Only a client gone longer than the TTL has its rule cleaned; on a later wake it
  must re-`proxy` (the rule is not auto-recreated). Rule removal does not unpatch the sidecar.

## 4. Files

| File | Role |
|---|---|
| `pkg/daemon/rpc/daemon.proto` | `WatchNamespaceRoutes` RPC + `NamespaceRoutes*` / `ServiceRecord` messages |
| `pkg/xds/route_discovery.go` | per-ns informers, Transform, `/24`//64 aggregation, snapshot/delta broadcaster, RPC impl |
| `pkg/xds/server.go` | registers the gzip compressor |
| `pkg/handler/network.go` | `StartRouteWatcher`, `AddRouteCIDR`; removed `AddRouteDynamic`/`watchAndRoute` |
| `pkg/dns/*.go` | `dns.Config.UpdateServices` + `applyResolvers`; removed `SvcInformer` |
| `pkg/handler/traffmgr_resources.go`, `traffmgr.go`, `connect.go` | `-route` Role/RoleBinding + `ensureRouteRBAC` |

## 5. Not done / limitations

- IPv6 aggregation uses a fixed `/64`; unusual pod IPv6 layouts may over/under-cover.
- Central-mode workload-namespace `-route` RBAC is not garbage-collected on uninstall
  (harmless dangling binding).
- Full end-to-end routing/DNS is only observable in-cluster (CI minikube); local arm64 TUN
  cannot reach a remote cluster.
