# Mesh Origin Return-Path over Loopback (declared ports)

## 1. Problem

In mesh mode, the injected sidecar envoy splits inbound traffic per `--headers` rule:
matched requests are tunneled to the developer's TUN IP; **unmatched requests must reach the
real application** (the original container in the same pod). envoy did this through a single
`origin_cluster` of type `ORIGINAL_DST` (`pkg/xds/cache.go`), i.e. it reconnected to the
**original destination = pod IP**.

That reconnection silently relies on a kernel behavior: "a connection to the pod's own IP is
routed through `lo`". On native Linux this holds, so `ORIGINAL_DST → podIP` is effectively a
loopback call and never re-enters the sidecar's `PREROUTING` DNAT. But on the lima/colima VM
kernel used by the macOS CI runner, `pod → own-IP` is routed through `eth0`, which re-enters
`PREROUTING`, re-matches the `DNAT … --to :15006` rule, and loops back into envoy — surfacing
as `503 upstream connect error` / `connection refused` (see
[40-envoy-original-dst-loop.md](40-envoy-original-dst-loop.md)). The `-s $POD_IP -j ACCEPT`
loop guard in `pkg/inject/container.go` was added to break that loop, but `ORIGINAL_DST →
podIP` also failed to reach the app in some ServiceIP paths (envoy access log
`cluster=origin_cluster upstream=<podIP>:9080 flags=UF`).

## 2. Fix

For **declared ports** (the workload's own container ports, which are known up front), route
the origin (no-header-match) return path to `127.0.0.1:<containerPort>` via a per-port
**STATIC** cluster, instead of `origin_cluster` (ORIGINAL_DST → podIP).

```
loopback_<containerPort>: STATIC cluster, endpoint 127.0.0.1:<containerPort>
```

`127.0.0.0/8` is hard-routed to `lo` by every kernel, so:

- the return connection never traverses `PREROUTING` → no ORIGINAL_DST loop, on any kernel;
- it deterministically reaches the local app (assuming the app listens on `0.0.0.0`/`127.0.0.1`),
  rather than depending on how the original destination was NAT'd.

This mirrors **Istio**, whose inbound clusters for declared ports point at `127.0.0.1:<targetPort>`
and only fall back to an `ORIGINAL_DST` passthrough for undeclared ports. It also mirrors KubeVPN's
own **fargate** mode, which already uses explicit per-IP container-port clusters because
`use_original_dst` does not work there.

## 3. What changed (`pkg/xds/cache.go`)

- `loopbackClusterName(port)` / `loopbackCluster(port, enableIPv6)`: per-port STATIC cluster whose
  endpoint is `127.0.0.1:port`, plus an `[::1]:port` `AdditionalAddresses` entry when IPv6 is enabled
  (envoy happy-eyeballs — see §5).
- `addTCPPort` (mesh branch): the default route (no header match) now targets `loopback_<containerPort>`,
  and the cluster is added to the snapshot.
- `buildFilterChains(routeName, tcpFallbackCluster)`: the raw-TCP fallback filter chain of each
  per-port listener now proxies to the per-port loopback cluster (mesh) instead of `origin_cluster`.
  `toListener` passes `loopback_<port>` for mesh and keeps `origin_cluster` for fargate.

Unchanged:

- `origin_cluster` (ORIGINAL_DST) and the `:15006` capture listener's **passthrough** still use it —
  it handles **undeclared ports**, whose port number is unknown so a static loopback cluster cannot
  be pre-built.
- The `-s $POD_IP -j ACCEPT` loop guard (`pkg/inject/container.go`) is **kept**: declared ports no
  longer depend on it, but the undeclared-port `ORIGINAL_DST` passthrough still can loop on
  lima/colima, and the rule is a harmless no-op on native Linux. Removing it can be evaluated
  separately once undeclared-port passthrough is handled (or deemed out of scope).

## 4. Data path (declared port, no header match)

```
user → TUN → traffic-manager → ClusterIP:9080
  → kube-proxy DNAT → pod_IP:9080
  → pod eth0 → iptables PREROUTING DNAT → envoy :15006 capture
  → use_original_dst → per-port listener (9080)
  → no header match → default route → loopback_9080 (STATIC)
  → envoy connects 127.0.0.1:9080  ← loopback, never re-enters PREROUTING
  → real app
```

## 5. IPv6 (happy-eyeballs)

When IPv6 is enabled (`processor.go` derives `enableIPv6` from `netutil.DetectSupportIPv6()` and
passes it to `Virtual.To`), the loopback endpoint carries **both** families on one endpoint:

- primary `Endpoint.Address` = `127.0.0.1:<port>`
- `Endpoint.AdditionalAddresses` = `[ [::1]:<port> ]`

Envoy's **happy-eyeballs** then attempts `127.0.0.1` and `::1` concurrently and uses whichever
connects first. So the origin return path reaches the app whether it listens on v4 (`0.0.0.0`),
v6 (`::`), or dual-stack — and unlike a round-robin over two endpoints, it never fails half the
requests when the app only binds one family. With IPv6 disabled, only `127.0.0.1` is emitted.

## 6. Limitations / assumptions

- **App bind address**: assumes the app listens on `0.0.0.0`/`127.0.0.1` (or `::`/`::1`), i.e. a
  loopback-reachable address — the default for virtually all servers. An app bound only to a
  specific pod IP is not reachable over loopback.
- **Undeclared ports**: still use `origin_cluster` (ORIGINAL_DST) passthrough, so they retain the
  old behavior (fine on Linux; relies on the loop guard on lima/colima).

## 7. Alternatives considered (and how Istio does it)

| Approach | Idea | Why not chosen (here) |
|---|---|---|
| **Istio's model** | Declared ports → inbound cluster to `127.0.0.1:<targetPort>`; undeclared → `InboundPassthroughCluster` (`ORIGINAL_DST`). Envoy runs as uid `1337` and an OUTPUT rule `-m owner --uid-owner 1337 -j RETURN` exempts envoy's own traffic; envoy→app goes over **loopback**, never re-entering PREROUTING. | This is exactly what we mirror — the chosen fix is Istio's "declared port → loopback" half. We don't adopt the uid-owner half (next row). |
| **uid-owner exemption (like Istio)** | Run envoy under a fixed uid, `iptables -m owner --uid-owner <uid> -j ACCEPT`. | iptables `owner` match is **only valid in OUTPUT/POSTROUTING**. KubeVPN's loop happens in **PREROUTING** (colima routes `pod→own-IP` via `eth0`, re-entering PREROUTING), where there is no socket owner to match. Istio escapes this only because its envoy→app stays on loopback (OUTPUT) and never reaches PREROUTING. |
| **`src==dst` loop guard** | Tighten the existing `-s $POD_IP -j ACCEPT` to `-s $POD_IP -d $POD_IP -j ACCEPT` (pod talking to itself). | More precise and covers all ports, but stays in PREROUTING. If colima RSTs/SNATs the hairpin *before* it returns to the pod, the guard never matches (consistent with the observed `connection refused`, not a loop `timeout`). Kept as the existing `-s $POD_IP` fallback for **undeclared-port** passthrough; not the primary fix. |
| **Route-layer force-to-lo** | Startup `ip route replace local ${POD_IP} dev lo` so `pod→own-IP` returns to `lo`, never hitting PREROUTING — covers all ports, no envoy change. | Needs `iproute2` added to the image, and whether it overrides colima's anomalous `local` route is unverified (CI-only). A candidate for undeclared ports if needed. |
| **`set_filter_state` rewrite** | A network filter rewrites `ORIGINAL_DST` to `127.0.0.1:%DOWNSTREAM_LOCAL_PORT%` (keep port, force loopback). | The `set_filter_state` proto is **not vendored** (go-control-plane v0.13.4); hand-rolling a raw `Any` or bumping the dep is fragile. |
| **Chosen: per-declared-port STATIC loopback cluster** | `loopback_<port>` → `127.0.0.1:port` (+ `::1` happy-eyeballs). | No image change; `127.0.0.0/8`/`::1` hard-route to `lo` on **any** kernel; aligns with Istio's declared-port path. Undeclared ports keep `ORIGINAL_DST` + the loop guard. |

**Why not just touch the DNAT rule.** The envoy return connection and a genuine inbound request share the **same destination** (the pod IP), so no `-d`-based DNAT predicate can tell them apart — only the **source** can (hence the `-s $POD_IP` guard). And any PREROUTING-level exemption only helps if the hairpin actually comes back into the pod; routing it to `lo` (loopback cluster, or route-layer fix) sidesteps PREROUTING entirely, which is why it is the more robust direction.

## 8. Tests

- `pkg/xds/cache_test.go`: `TestVirtual_To_MeshDefaultRouteLoopback` (default route + STATIC
  endpoint `127.0.0.1:port`, no additional address when v6 off);
  `TestVirtual_To_MeshLoopbackDualStack` (v6 on → primary `127.0.0.1` + additional `::1`);
  capture passthrough still `origin_cluster` (`TestVirtual_To_MeshInboundCaptureListener`);
  empty-TUN-IP and empty-rules cases updated.
- `pkg/xds/envoy_e2e_test.go`: real-Envoy mesh capture listener still binds `:15006`.
- Full mesh routing (ServiceIP 503 disappearance) is only observable in-cluster (CI minikube/lima),
  since local arm64 TUN cannot reach the remote cluster.

## 9. Related

- [16-envoy-controlplane.md](16-envoy-controlplane.md) — xDS control plane, clusters, capture listener
- [17-sidecar-injection.md](17-sidecar-injection.md) — sidecar injection strategies and iptables rules
- [40-envoy-original-dst-loop.md](40-envoy-original-dst-loop.md) — the ORIGINAL_DST loop on non-Linux kernels
