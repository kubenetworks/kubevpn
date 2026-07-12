# 46. Server-Side CIDR & DNS Detection (design)

## Context

`kubevpn connect` currently performs two *cluster-view* discovery steps **on the client**
(root daemon, during `DataSession.DoConnect`):

1. **Cluster CIDR detection** — `DataSession.getCIDR` (`pkg/handler/data_session.go:290`)
   → `util.GetCIDR` (`pkg/util/cidr_detect.go`): creates a probe pod, `exec`s into it to
   grep `/proc`, lists pods in kube-system and the target namespace, and creates a bogus
   Service to parse the Service-CIDR out of the API error.
2. **Cluster DNS detection** — `NetworkManager.setupDNS` (`pkg/handler/network.go`) →
   `util.GetDNSServiceIPFromPod`: `exec`s into the manager pod to read its
   `/etc/resolv.conf`, plus `listResolvableNamespaces` lists all namespaces.

Both are "same for every client" cluster facts, yet each client computes them, which:
- requires **client RBAC** for `pods/create`, `pods/exec`, `services/create`;
- adds first-connect latency (probe pod create + wait + delete, remote `exec`).

Goal: move the *detection* into the in-cluster traffic-manager so the client only
consumes the result. This is a **complexity / permission-surface** reduction, not a memory
one (client memory is addressed separately by capping `pkg/core` `MaxSize`). CIDR results
are already cached in the traffic-manager ConfigMap (`CLUSTER_CIDRS`), so steady-state
client cost is already low; the win is the **first connect** and dropping client RBAC.

This mirrors `44-server-side-route-discovery.md`: cluster-view work belongs on the server,
per-client decisions stay on the client.

## A subtlety that must be fixed first (cache contract)

Today the cache is **not** raw CIDRs. `getCIDR` writes
`encodeCIDRs(dedupAndFilterCIDRs(raw, apiServerIPs))` (`data_session.go:308`) — i.e. CIDRs
**already filtered by the first client's API-server IPs**. API-server IPs differ per client
(kubeconfig host / SSH topology, see `32-cidr-detection.md`), so a per-client-agnostic
server writer must NOT store a filtered set.

**Required change:** the cache stores **raw** CIDRs; every reader filters on read with its
own API-server IPs via `parseCachedCIDRs` (which already filters). The client write path
(`data_session.go`) must also switch to writing raw. `parseCachedCIDRs` already filters, so
this is safe and is strictly more correct than the current "filtered by whoever wrote first".

## Design

### C1 — CIDR detection in the manager (additive, degrade-safe)

- Host: the **`xds` container's `TunConfigServer`** (`pkg/xds`) — the in-cluster control
  plane that already holds a clientset and starts before any client connects. No import
  cycle (`pkg/xds` does not import `pkg/handler`; both may import `pkg/util`/`pkg/config`).
- On startup, if `CLUSTER_CIDRS` is empty, run detection **in-cluster** and write the **raw**
  CIDRs. Detection uses `util.GetClusterCIDRNoProbePod` — the pod-free strategies only
  (kube-system component flags, a rejected Service create, and pod CIDR inferred from existing
  pod IPs): **no probe pod, no exec**, since the manager is already an in-cluster pod.
- Client: **unchanged** read path + local fallback (`data_session.go:297`). Because the
  manager warms the cache, the client's `util.GetCIDR` effectively never runs; if the manager
  is old / detection failed / RBAC missing, the cache is empty and the client falls back to
  today's behavior. **Purely additive — cannot break connectivity.**
- Per-client API-server-IP filtering stays on the client (`parseCachedCIDRs`).
- RBAC: extend the manager Role (`traffmgr_resources.go`) with `pods list` and `services create`
  (no `pods/create`, no `pods/exec` — detection is pod-free). **RBAC failure degrades safely** (cache
  stays empty → client fallback).

### C2 — DNS detection in the manager (piggyback, no proto change)

Scope: move **only** the cluster-DNS fact that requires an `exec` — the DNS server IP and
search domains read from the manager pod's `/etc/resolv.conf`. **Namespace enumeration stays
on the client** (see below).

- The manager reads its **own** `/etc/resolv.conf` (a local file — zero API cost) and writes
  the cluster DNS server IP + search domains into a new ConfigMap key (reuse the
  ConfigMap-cache mechanism — **no proto/RPC change**).
- Client: read that key instead of `exec`ing into the pod; fall back to today's `exec` when
  the key is absent (old manager). The per-client **ClusterIP-vs-PodIP connectivity probe**
  (`detectNameserver`) stays on the client (it depends on client→cluster reachability).
- **`listResolvableNamespaces` stays on the client.** The set of resolvable namespaces is a
  per-client view: it depends on the client's connect scope / namespace model
  (`07-namespace-model.md`) and the client's own RBAC. Moving it to the manager would force
  the manager SA to hold cluster-wide `namespaces list` and could surface namespaces the
  client is not entitled to resolve. Keep it client-side.

## Stays on the client (non-goals)

TUN device + local gvisor stack, local route table writes, `/etc/hosts`/resolver writes,
port-forward, health check, local SOCKS/HTTP proxy, syncthing local end, **API-server-IP
filtering**, the **DNS connectivity probe**, and **namespace enumeration**
(`listResolvableNamespaces`) — all are per-client / host-local views.

## Compatibility & rollout

Both parts are additive with a client fallback, so a new client works against an old manager
(falls back) and an old client works against a new manager (ignores the extra key). No
version gate needed.

## Validation plan

Unit (no cluster):
- Inject the detector so the warm-up's ConfigMap write/skip logic is testable with a fake
  clientset (write-when-empty, skip-when-present, no-op-on-detect-error).
- Client: reads raw cache + filters per-client; falls back when key absent (fake clientset).

Integration / CI e2e (minikube — cannot run in the dev sandbox):
- Fresh cluster, first `connect`: manager warms `CLUSTER_CIDRS`; assert the **client creates
  no probe pod** and routes come up.
- Remove the manager's new RBAC: assert client still connects (fallback path).
- DNS: assert client resolves cluster services without `exec`ing the manager pod.

## Status

Implemented (unit-tested, additive, degrade-safe) — **pending CI minikube e2e**:

- **C1 cache contract**: `data_session.getCIDR` now caches **raw** (deduped, unfiltered) CIDRs.
- **C1 warm-up**: `TunConfigServer.WarmClusterCIDRCache` (`pkg/xds/tun_config_cidr.go`), started
  from `xds.Main`; fills an empty `CLUSTER_CIDRS` in-cluster via `util.GetClusterCIDRNoProbePod`
  (no probe pod / no exec).
- **C1 RBAC**: `genRole` grants the manager SA `pods list` + `services create` in
  the manager namespace (fresh installs; existing managers keep their Role until recreated —
  warm-up degrades safely meanwhile).
- **C2**: `TunConfigServer.WarmClusterDNSCache` (`pkg/xds/tun_config_dns.go`) publishes the
  manager pod's `/etc/resolv.conf` to `CLUSTER_DNS_RESOLV`; `util.GetDNSServiceIPFromPod`
  reads that key first and falls back to `exec`. Namespace enumeration stays on the client.
  Timing: **both** the CIDR and DNS warm-ups run **synchronously in `xds.Main` before the
  server serves** (bounded 10s / 5s), so both caches are present before the first client
  connects (no fallback race). Both are cheap now — no probe pod / no exec, just a few API
  calls and a local file read. Each writes once, only when its key is empty; the value
  persists across manager restarts.

> **CI e2e is still required before relying on it:** correctness of the in-cluster detection
> (parity with client detection), the RBAC grant, and the existing-manager rollout are only
> verifiable against a real cluster, which the dev sandbox cannot run. Everything is additive
> with a client fallback, so a failure there degrades to today's client-side behavior.
