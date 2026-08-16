# Envoy ORIGINAL_DST Loop on Non-Linux Kernels

## 1. Problem

In mesh mode, envoy sidecars on **colima** (and potentially other non-Linux Docker backends on macOS) return `503 Service Unavailable` with "connection refused" for traffic that should reach the real application via `origin_cluster`. The same configuration works correctly on native Linux (including minikube on Linux).

## 2. Root Cause

### 2.1 Data path

```
user → TUN → traffic-manager → ClusterIP:9080
  → kube-proxy DNAT → pod_IP:9080
  → pod eth0
  → iptables PREROUTING: DNAT to envoy :15006
  → envoy inbound capture listener
  → origin_cluster (ORIGINAL_DST)
  → envoy connects to original dest: pod_IP:9080
```

### 2.2 The loop

When envoy's `origin_cluster` connects to `pod_IP:9080`:

| Kernel | Local-to-local routing | PREROUTING re-entry | Result |
|---|---|---|---|
| Native Linux | via **loopback** (`lo`) | No (loopback bypasses PREROUTING) | App responds normally |
| Colima VM (Lima) | via **physical interface** (`eth0`) | Yes — DNAT rule matches again | Envoy → DNAT → envoy → ... → timeout → 503 |

On Linux, the kernel recognizes that `pod_IP → pod_IP` is a local connection and routes it through the loopback interface. Loopback traffic does not traverse the PREROUTING chain, so the DNAT rule is not re-applied.

On colima's VM kernel, the same connection is routed through `eth0`. Traffic on `eth0` traverses the full netfilter PREROUTING chain, hits the DNAT rule again, and redirects back to envoy — creating an infinite loop.

### 2.3 Which macOS Docker backend triggers this

The trigger is the **lima** VM kernel (QEMU guest), which routes a pod's connection to its own IP
through `eth0` instead of `lo`. This affects both `colima` (lima + docker) and — importantly —
`docker/setup-docker-action@v4` on the `macos-15-intel` runner: that action does **not** provide a
Docker Desktop linuxkit daemon there; it `brew install lima` and runs docker inside a lima VM
(`docker-actions-toolkit`). So switching the workflow back to `setup-docker-action` does **not**
escape this kernel difference — it is still lima. Only a true linuxkit/Docker-Desktop or native
Linux kernel routes pod→own-IP through `lo`.

> Earlier revisions of this doc claimed `setup-docker-action@v4` == Docker Desktop linuxkit (safe);
> CI logs show it is lima on the macOS runner, i.e. same kernel family as colima.

### 2.4 Fix for declared ports: loopback return path

For **declared ports**, the origin (no-header-match) return path is sent to
`127.0.0.1:<containerPort>` via a per-port STATIC cluster instead of `ORIGINAL_DST → podIP`.
Loopback is hard-routed to `lo` on every kernel, so it never re-enters `PREROUTING` and the loop
cannot form regardless of backend. See [42-origin-loopback-cluster.md](42-origin-loopback-cluster.md).

## 3. Fix

### 3.1 Loopback cluster (declared ports)

The fix lives entirely in the xDS control plane (`pkg/xds/cache.go`): the per-port listener's
default route and raw-TCP fallback target a per-port STATIC `loopback_<port>` cluster whose endpoint
is `127.0.0.1:<containerPort>`. No iptables change and no image change are required — `127.0.0.0/8`
(and `::1`) hard-route to `lo` on any kernel, so the return connection sidesteps `PREROUTING`
entirely. Details, IPv6 happy-eyeballs, and the alternatives considered are in
[42-origin-loopback-cluster.md](42-origin-loopback-cluster.md).

### 3.2 Historical iptables loop guard (removed)

Earlier revisions broke the loop with a PREROUTING source-match guard in the VPN sidecar startup
script (`iptables -t nat -I PREROUTING 1 -p tcp -s ${POD_IP} -j ACCEPT`, before the DNAT rule). It
let envoy's `ORIGINAL_DST → podIP` return connection bypass DNAT by matching its source = pod IP.

That guard has been **removed**. Once declared ports return over the loopback cluster they no longer
depend on it, and on native Linux it was always a no-op (pod→own-IP routes through `lo`, never
entering PREROUTING). Its only remaining beneficiary was the **undeclared-port** passthrough
(§3.3) — a niche case whose residual risk was accepted in exchange for a simpler sidecar script and
removing the now-orphaned `POD_IP` downward-API env. The guard's effectiveness on colima was itself
uncertain (see [42-origin-loopback-cluster.md](42-origin-loopback-cluster.md) §7).

### 3.3 Residual risk: undeclared ports

Ports the app listens on but does **not** declare in the pod spec have no per-port loopback cluster
(the port is unknown at config-build time). They fall through the `:15006` capture listener to the
`origin_cluster` (`ORIGINAL_DST → podIP`) passthrough, which can still loop on lima/colima kernels.
Covering them would require either `set_filter_state` (rewrite ORIGINAL_DST to
`127.0.0.1:%DOWNSTREAM_LOCAL_PORT%` — proto not vendored in go-control-plane v0.13.4) or a
route-layer `ip route replace local ${POD_IP} dev lo` (needs `iproute2` in the image, unverified on
colima). Both are out of scope; see [42-origin-loopback-cluster.md](42-origin-loopback-cluster.md)
§7. On native Linux there is no residual risk.

## 4. Related

- [17-sidecar-injection.md](17-sidecar-injection.md) — sidecar injection strategies and iptables rules
- [16-envoy-controlplane.md](16-envoy-controlplane.md) — envoy xDS control plane, origin_cluster, and inbound capture listener
- [18-gvisor-network-stack.md](18-gvisor-network-stack.md) — gVisor TCP/UDP forwarding
