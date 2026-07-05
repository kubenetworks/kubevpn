# Cluster CIDR Detection

## 1. Overview

To build the routing table on the client and to configure the gvisor stack server-side,
KubeVPN needs to know the cluster's **Pod CIDRs** and **Service CIDR**. Kubernetes does not
expose these as a first-class API, and different clusters (kubeadm, ACK, EKS, Calico, kindnet…)
surface them in different places. KubeVPN therefore runs a **best-effort, multi-strategy
detection pipeline**: each strategy contributes whatever CIDRs it can find, the union is
deduplicated, and the result is cached in the traffic manager ConfigMap so reconnects skip
detection entirely.

Detection is a **data-plane** responsibility — it runs inside `DoConnect` (root daemon),
because the CIDRs feed directly into TUN route setup and the gvisor stack. See
[02-dual-daemon.md](02-dual-daemon.md).

Entry points:
- `util.GetCIDR()` — the raw multi-strategy detector (`pkg/util/cidr.go`)
- `ConnectOptions.getCIDR()` — the caching wrapper (`pkg/handler/connect.go`)

## 2. Architecture

```
ConnectOptions.getCIDR(ctx)                         pkg/handler/connect.go
  │
  ├── getAPIServerIPs()  ── resolve kubeconfig Host + SSH jump hosts → []net.IP
  │
  ├── Get(CLUSTER_CIDRS) from ConfigMap ──► non-empty? ─► parseCachedCIDRs() ─► RETURN (cached)
  │
  └── cache miss ─► util.GetCIDR(ctx, clientset, restconfig, managerNS, image)
                      │
                      ├── (1) GetCIDRByDumpClusterInfo   ── kube-system pod flags
                      ├── (2) GetCIDRFromCNI             ── helper pod: grep host /proc/*/cmdline
                      ├── (3) GetPodCIDRFromCNI          ── helper pod: parse /etc/cni/net.d/*.conflist
                      ├── (4) GetServiceCIDRByCreateService ── invalid-ClusterIP error trick
                      └── (5) GetPodCIDRFromPod          ── infer /16 or /64 from real Pod IPs
                      │
                      ▼
                   raw []*net.IPNet
                      │
            dedupAndFilterCIDRs(raw, apiServerIPs)
              = RemoveLargerOverlappingCIDRs ∘ RemoveCIDRsContainingIPs
                      │
            Set(CLUSTER_CIDRS, encodeCIDRs(cidrs))  ── write back to cache
                      ▼
                  RETURN cidrs, apiServerIPs
```

## 3. Detection Strategies

All five run in sequence; failures are non-fatal (logged at Debug) and simply contribute nothing.

### Strategy 1 — Component flags (`GetCIDRByDumpClusterInfo`)

Lists up to 100 pods in `kube-system` and scans every container's `Args`/`Command` for
`--cluster-cidr` and `--service-cluster-ip-range` (kube-apiserver, kube-controller-manager,
kube-proxy). `parseCIDRFromFlag` splits on `=` and parses each comma-separated CIDR. This is
the cheapest and most authoritative source when the control-plane pods are visible.

### Strategy 2 — Host cmdline via helper pod (`GetCIDRFromCNI`)

When the component flags are not visible (managed clusters hide the control plane), KubeVPN
creates a **helper pod** (`CreateCIDRPod`) that mounts the host `/proc` (at `/etc/cni/proc`) and
`/etc/cni/net.d`, then runs:

```sh
grep -a -R "service-cluster-ip-range\|cluster-cidr" /etc/cni/proc/*/cmdline \
  | grep -a -v grep | tr "\0" "\n"
```

Each matching line is fed back through `parseCIDRFromFlag`.

### Strategy 3 — CNI conflist (`GetPodCIDRFromPod` → `GetPodCIDRFromCNI`)

`GetPodCIDRFromCNI` reads `/etc/cni/net.d/*.conflist` from the helper pod and parses it with
`libcni.ConfListFromBytes`. For Calico plugins it extracts `ipam.ipv4_pools` and
`ipam.ipv6_pools` via `unstructured.NestedStringSlice`, covering both IPv4 and IPv6 pool ranges.

### Strategy 4 — Service-CIDR error trick (`GetServiceCIDRByCreateService`)

The Service CIDR is never exposed directly, so KubeVPN provokes the API server: it creates a
Service with `ClusterIP: 0.0.0.0` (always invalid). The API server rejects it with a message
containing the valid range, e.g. *"The range of valid IPs is 10.96.0.0/12"*. The error text is
scanned for the keywords `valid IPs is`, `The range of valid IPs`, `valid IP range is`, and the
trailing CIDR is parsed.

### Strategy 5 — Infer from real Pod IPs (`GetPodCIDRFromPod`)

A fallback that lists up to 100 pods, skips `HostNetwork` pods, and for every `PodIP`/`PodIPs`
applies a default mask — **/16 for IPv4, /64 for IPv6** — to derive a covering CIDR. The exact
mask is unknown without `node.Spec.PodCIDR`, so these defaults are intentionally broad.

## 4. The Helper Pod (`CreateCIDRPod` / `buildCIDRPodSpec`)

| Property | Value |
|---|---|
| Name | `config.CniNetName` = `cni-net-dir-kubevpn` |
| Image | the traffic-manager image (so it is already pulled) |
| Command | `tail -f /dev/null` (idle) |
| Volumes | host `/etc/cni/net.d` (`config.DefaultNetDir`) and host `/proc` (`config.Proc`), both read-only |
| Scheduling | prefers control-plane nodes (`node-role.kubernetes.io/{master,control-plane}` affinity + tolerations), spread by hostname |
| Resources | 16m CPU / 16Mi memory request+limit |
| Wait | created if absent or not Running; `WaitPod` with a 15s timeout |
| Teardown | deleted in `GetCIDR`'s `defer` with `GracePeriodSeconds=0` |

## 5. Dedup & Filtering

The raw union is post-processed by `dedupAndFilterCIDRs`:

- **`RemoveLargerOverlappingCIDRs`** — sorts by prefix length (broadest first) and drops any CIDR
  contained in / containing an already-kept CIDR, so only the widest range in each overlap group
  survives.
- **`RemoveCIDRsContainingIPs`** — drops any CIDR that contains an **API server IP**. The API
  server must remain reachable through the host's normal route, not be hijacked into the TUN, so
  its IPs (and SSH jump host IPs) are carved out. API server IPs come from `GetAPIServerIP`
  (kubeconfig `Host` parse + DNS lookup) plus `c.SshHosts`.

## 6. Caching (`CLUSTER_CIDRS` ConfigMap key)

Detection touches the API server several times and creates a pod, so the result is cached:

- On success, `encodeCIDRs` writes a deduplicated space-separated string to the
  `config.KeyClusterCIDRs` = `CLUSTER_CIDRS` key of the traffic manager ConfigMap (via
  `ConfigMapStore.Set`). The key is pre-created empty in `createOutboundPod` (`traffmgr.go`).
- On the next connect, a non-empty value short-circuits the whole pipeline: `parseCachedCIDRs`
  re-parses it, re-applies dedup + API-server filtering (the API server IPs may differ per
  client), and the step reports `Detected cluster CIDRs: … (cached)`.
- An empty detection result is **not** cached, so a transient failure does not poison future runs.

`pkg/handler/once.go` reuses `getCIDR` during the Helm `once` bootstrap to warm this cache.

## 7. Edge Cases

- All strategies may legitimately return nothing (e.g. extremely locked-down clusters); `getCIDR`
  then returns an empty slice — the connection still proceeds, relying on explicit
  `--extra-route` / `ExtraRouteInfo` and DNS-driven routing.
- Dual-stack clusters yield both IPv4 and IPv6 CIDRs; both flow through unchanged. See
  [38-ipv6-dual-stack.md](38-ipv6-dual-stack.md).
- The same dedup/filter pair is reused on the network side in `network.go` against
  `nm.cfg.APIServerIPs`, keeping client and server views consistent.

## 8. Related Files

| File | Purpose |
|---|---|
| `pkg/util/cidr.go` | Pure helpers: `CIDRsToString`, `parseCIDRFromFlag`, `RemoveCIDRsContainingIPs`, `RemoveLargerOverlappingCIDRs`, `GetAPIServerIP` (no cluster I/O — unit-tested without a kubeconfig) |
| `pkg/util/cidr_detect.go` | Cluster-I/O detectors: `GetCIDR` and the five strategy functions, `CreateCIDRPod`, `buildCIDRPodSpec` |
| `pkg/handler/connect.go` | `getCIDR` caching wrapper, `parseCachedCIDRs`, `encodeCIDRs`, `dedupAndFilterCIDRs`, `getAPIServerIPs` |
| `pkg/handler/once.go` | Warms the CIDR cache during Helm `once` bootstrap |
| `pkg/handler/traffmgr.go` | Pre-creates the `CLUSTER_CIDRS` ConfigMap key |
| `pkg/config/config.go` | `KeyClusterCIDRs`, `CniNetName`, `DefaultNetDir`, `Proc` constants |

## 9. Related Docs

- [03-dhcp-ip-allocation.md](03-dhcp-ip-allocation.md) — `CLUSTER_CIDRS` lives alongside `TUN_IP_POOL`/`TUN_ALLOCS` in the same ConfigMap
- [07-namespace-model.md](07-namespace-model.md) — detection runs against the manager namespace
- [01-network-architecture.md](01-network-architecture.md) — how detected CIDRs become routes
- [38-ipv6-dual-stack.md](38-ipv6-dual-stack.md) — dual-stack CIDR handling
