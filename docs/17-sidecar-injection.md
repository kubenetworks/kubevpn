# Sidecar Injection Design

## 1. Overview

The inject package (`pkg/inject`) manages the lifecycle of sidecar containers injected into Kubernetes workloads. It uses the **Strategy pattern** to choose between two injection modes based on the target workload. It also manages envoy routing rules in the traffic manager ConfigMap.

> **Unified proxy mode:** there is no longer a separate VPN-only injector. VPN-only proxying (`kubevpn proxy` without `--headers`) is just Mesh mode with **empty headers** — for every **declared container port** envoy matches all requests and forwards them to the user's TUN IP. This eliminates the old hardcoded iptables-DNAT-to-client-IP path and lets all routing (TCP + UDP) hot-update via envoy xDS. See [28-sleep-wake-ip-update.md](28-sleep-wake-ip-update.md).
>
> **Caveat — this is not a full equivalent of the old VPN-only sidecar.** Mesh mode only intercepts *declared* ports (those in the workload's Pod spec / `--portmap`); traffic to *undeclared* ports falls through to the real application, not to your machine. The old VPN-only sidecar forwarded **all** ports. See the **Full-proxy port coverage** section below for the mechanism and the options to restore all-port passthrough.

## 2. Strategy Pattern

```go
type Injector interface {
    Inject(ctx context.Context) error
}
```

`NewInjector(opts)` selects the strategy:

| Condition | Strategy | Containers Injected |
|-----------|----------|-------------------|
| Target is a K8s Service | `fargateInjector` | SSH + Envoy |
| Otherwise (any non-Service workload) | `meshInjector` | VPN + Envoy |

`meshInjector` handles both header-based traffic splitting (`--headers` set) **and** full VPN-only interception (headers empty). Both strategies share the same `InjectOptions` parameter object:

```go
type InjectOptions struct {
    Factory          cmdutil.Factory
    Clientset        kubernetes.Interface
    ManagerNamespace string       // where ConfigMap lives
    NodeID           string       // group.resource.name identifier
    Object           *resource.Info  // target (may be Service)
    Controller       *resource.Info  // owning controller
    LocalTunIPv4     string
    LocalTunIPv6     string
    Headers          map[string]string
    PortMaps         []string
    Secret           *v1.Secret   // TLS secret
    Image            string
    OwnerID          string
}
```

## 3. Injection Strategies

### 3.1 Mesh (`meshInjector`) — covers VPN-only and header splitting

Used for `kubevpn proxy`, with or without `--headers`.

```
Inject:
  1. collectPorts → envoy ports + portmap
  2. addEnvoyConfig → write Virtual/Rule to ConfigMap
  3. Check if already injected *for the current manager namespace* (skip container add)
  4. AddVPNAndEnvoyContainer → inject VPN + Envoy sidecars
  5. patchWorkload → JSON patch the controller
```

Mesh mode adds two containers:
- **VPN container**: enables IP forwarding and DNATs all non-ICMP, non-cluster traffic to envoy's inbound port `:15006` (a *fixed* port — no client-IP in the rule). Runs `kubevpn server -l "tun:/tcp://...:10801?route=${CIDR4}"` with `net=` empty, so it self-allocates its TUN IP from the control-plane at startup. Requires `NET_ADMIN` + `NET_RAW` capabilities (runs as root but not privileged).
- **Envoy container**: runs envoy with xDS config pointing to the control plane; routes by header.

**Routing behavior depends on whether headers are set:**

| Headers | envoy route | Effect |
|---------|-------------|--------|
| Empty (VPN-only) | `RouteMatch{Prefix:"/"}` matches everything **on each declared port's listener** | All traffic **on declared ports** forwarded to the user's TUN IP. Undeclared ports have no listener and fall through the `:15006` capture to `origin_cluster` (the real app) — see Full-proxy port coverage below |
| Set (`--headers`) | header match → user IP; no match → `origin_cluster` | Per-user traffic splitting; unmatched requests hit the real app |

Multiple users can proxy the same workload with different headers — envoy routes each user's traffic to their own TUN IP. Because the routing target (`Rule.LocalTunIPv4/v6`) lives in `ENVOY_CONFIG`, a client IP change is picked up automatically via `syncEnvoyRuleIP` → xDS push (TCP + UDP), with no Pod restart.

### 3.2 Fargate/Service (`fargateInjector`)

Used when targeting a K8s Service (no `NET_ADMIN` capability available).

```
Inject:
  1. collectFargatePorts → allocate random envoy listener ports
  2. addEnvoyConfig → write Virtual/Rule with FargateMode=true
  3. Check if already injected *for the current manager namespace*
  4. AddEnvoyAndSSHContainer → inject SSH + Envoy sidecars
  5. patchWorkload → JSON patch the controller
  6. ModifyServiceTargetPort → redirect Service ports to envoy listeners
```

Key differences from mesh mode:
- **No VPN sidecar** — no `NET_ADMIN`, no iptables
- **SSH container** instead of VPN — runs `kubevpn server -l ssh://:2222`
- **Envoy binds directly** (`BindToPort=true`) to random ports
- **Service targetPort** is updated to point to envoy's listener ports
- **TUN IPs** are `127.0.0.1` / `::1` — traffic flows via SSH reverse tunnels

### 3.3 Full-proxy Port Coverage (vs. the old VPN-only sidecar)

> **Status: open design decision — no code change yet.** This section records a behavioral
> regression introduced by the unified-proxy refactor and the options to address it.

#### The difference

| | Old VPN-only sidecar (pre-refactor) | New mesh full-proxy (empty headers) |
|---|---|---|
| Mechanism | iptables DNATs **every** inbound port into the tunnel | envoy intercepts only **declared** container ports |
| Undeclared port (e.g. `:9090`) | forwarded to your machine | falls through to `origin_cluster` → the real app |
| Port coverage | all ports (L3/L4, port-agnostic) | declared ports only |

The refactor folded VPN-only proxying into mesh mode ("empty headers = match-all"). For
**declared** ports the two are equivalent — envoy's catch-all route sends everything to your
TUN IP. But the old sidecar was port-agnostic: it tunneled *any* port. Mesh mode is not, so
**full-proxy lost the "any port reaches my machine" semantic** for ports the workload never declared.

#### Mechanism

`Virtual.To()` (`pkg/xds/cache.go`) emits a per-port TCP listener **only** for the ports
in `a.Ports`, which `pkg/inject` populates from `collectPorts` / `gatherContainerPorts`
(`pkg/inject/injector.go`) — i.e. the Pod spec's declared ports plus any `--portmap` entries.

In mesh mode those per-port listeners are `BindToPort=false`, so the sidecar's iptables DNATs all
inbound TCP to a single virtual-inbound capture listener on `:15006`
(`toInboundCaptureListener`, `cache.go`). That listener restores the original destination and:

- **declared port** → redirect to its per-port listener → empty-headers catch-all route → your TUN IP;
- **undeclared port** → passthrough filter chain → `origin_cluster` (`ORIGINAL_DST`) → the real app.

Within a declared port's per-port listener, a request matching **no** header rule (the origin return
path) goes to `loopback_<containerPort>` (`127.0.0.1:<containerPort>`, a STATIC cluster), not
`origin_cluster`/`ORIGINAL_DST`. Loopback never re-enters `PREROUTING`, avoiding the ORIGINAL_DST loop
on lima/colima kernels and reliably reaching the local app — see
[41-origin-loopback-cluster.md](41-origin-loopback-cluster.md). Option C below stays relevant only for
**undeclared** ports, which still use `ORIGINAL_DST`.

See [16-envoy-controlplane.md](16-envoy-controlplane.md) ("Mesh-mode inbound capture listener" and
"Routing logic") for the xDS detail.

#### Concrete symptom

`kubevpn run` / `kubevpn sync` e2e probes `podIP:9090`. In host mode the dev container is published
on `host:9090` and the gvisor `LocalTCPForwarder` dials `127.0.0.1` (`pkg/core/gvisor_tcp_forwarder.go`).
If `9090` is not a declared port, the request hits the `:15006` capture passthrough → `origin_cluster`
→ the real app, never the local dev process — so the probe fails. See [20-kubevpn-run.md](20-kubevpn-run.md).

#### Options to restore all-port passthrough

| Option | Idea | Pros | Cons |
|---|---|---|---|
| **A. Restore VPN-only injector** | `NewInjector` splits three ways again: empty headers → VPN-only (iptables all-port DNAT to client TUN), `--headers` → mesh, Service → fargate | True all-port passthrough; simplest; matches old semantics | Re-introduces `vpnInjector` + its proxy tracking/leave; conflicts with the "unified mesh" model; **loses envoy TUN-IP hot-update** (VPN-only iptables is static) |
| **B. Keep envoy + hybrid iptables** | Declared ports DNAT → `:15006` (match-all → TUN, keeps xDS hot-update); undeclared ports bypass envoy and the VPN sidecar transparently forwards them to local | Keeps hot-update **and** restores undeclared-port passthrough | iptables must be generated per declared-port set and kept in sync; one sidecar with two data paths; higher complexity |
| **C. Pure envoy (`set_filter_state`)** | A `set_filter_state` network filter rewrites the `ORIGINAL_DST` target to `TUN_IP:%DOWNSTREAM_LOCAL_PORT%` (fixed IP, original port) before TCP proxy | Would need no sidecar changes | **Not feasible here:** the `set_filter_state` proto is **not vendored** (`vendor/github.com/envoyproxy` has none; deps are go-control-plane v0.13.4 / envoy v1.35.0). Hand-rolling a raw `Any` or bumping the dependency is fragile. Envoy is fundamentally a per-port L4/L7 proxy with no off-the-shelf "rewrite IP, keep port" for arbitrary TCP |

**Insight:** a pure full-proxy (no headers) does not actually need envoy's splitting — envoy is just a
match-all → TUN forwarder there. So the cleanest restore is **A**; choose **B** only if envoy's
TUN-IP hot-update must be preserved for full-proxy.

## 4. Container Builders (`container.go`)

### Environment Variables

All sidecar containers receive:

| Env Var | Source | Purpose |
|---------|--------|---------|
| `CIDR4`, `CIDR6` | `config.CIDR` | VPN address pool |
| `TrafficManagerService` | `kubevpn-traffic-manager.{managerNs}` | gRPC endpoint (`{managerNs}` is the **manager** namespace, not the workload namespace — see [07-namespace-model.md](07-namespace-model.md)) |

> **Manager-namespace change forces re-injection.** The manager address above (and the
> identical address baked into the envoy `xds_cluster` bootstrap) is fixed at injection
> time. If the traffic-manager later moves namespaces (e.g. per-namespace → centralized),
> the "already injected" check (`injectedForManager`) compares the envoy sidecar's baked
> address against the *current* manager namespace and re-injects on mismatch — otherwise
> the sidecar's `xds_cluster` would keep pointing at a deleted Service whose DNS no longer
> resolves, permanently losing its xDS stream and serving stale routes.
| `POD_NAMESPACE` | Downward API | Current namespace |
| `POD_NAME` | Downward API | Current pod name |
| `tls_crt`, `tls_key`, `tls_server_name` | TLS Secret | mTLS credentials (VPN/mesh only) |

### Container Types

| Function | Containers | Security |
|----------|-----------|----------|
| `AddVPNAndEnvoyContainer` | VPN + Envoy (all non-Service proxy) | VPN: NET_ADMIN + NET_RAW (not privileged); Envoy: unprivileged |
| `AddEnvoyAndSSHContainer` | SSH + Envoy (Service/Fargate) | Both unprivileged |

> The former `AddVPNContainer` (VPN-only, no envoy) was **removed** with the unified proxy mode — every non-Service proxy now goes through `AddVPNAndEnvoyContainer`. Note this changed full-proxy's port-coverage semantic (declared ports only); see the Full-proxy port coverage section above.

### ServiceAccount

The injected pod keeps the **workload's own ServiceAccount** (whatever the original Deployment used, usually `default`). The sidecars deliberately do **not** set `spec.serviceAccountName` to the `kubevpn-traffic-manager` SA:

- That SA only exists in the **manager** namespace (see [24-traffic-manager-deployment.md](24-traffic-manager-deployment.md)). Pinning it onto a workload in any other namespace makes the ServiceAccount admission controller **reject pod creation** — the new ReplicaSet never produces a pod, the rollout times out, and `RolloutStatus` undoes the patch, leaving the workload on its original (un-injected) spec.
- The sidecars never call the K8s API: the VPN sidecar self-allocates its TUN IP from the control plane over **gRPC + TLS** (`requestTunIPFromControlPlane`, see [09-tun-ip-hot-update.md](09-tun-ip-hot-update.md)), authenticating with the `tls_*` env vars — so no ServiceAccount token is needed.

## 5. Envoy ConfigMap Management (`envoy.go` + `virtual_store.go`)

### Writing Rules

`addEnvoyConfig(ctx, configMapInterface, envoyRuleSpec)` is now a thin wrapper:

```go
func addEnvoyConfig(ctx, mapInterface, spec) error {
    return NewVirtualStore(mapInterface).AddRule(ctx, spec)
}
```

`VirtualStore.AddRule` performs the actual read-modify-write:
1. `RetryOnConflict` loop wrapping a GET → decode → mutate → encode → UPDATE
2. Decode `ENVOY_CONFIG` YAML → `[]*xds.Virtual`
3. Call `addVirtualRule(virtuals, spec)` to merge the new rule
4. Marshal back to YAML and UPDATE the ConfigMap (optimistic concurrency via `ResourceVersion`)

See [16-envoy-controlplane.md §5](16-envoy-controlplane.md) for the full `VirtualStore` design.

### Rule Merging Logic (`addVirtualRule`)

Four cases when adding a rule:

| Case | Condition | Action |
|------|-----------|--------|
| 1. New workload | No existing Virtual for this nodeID | Create new Virtual with one Rule |
| 2. Same owner update | Existing rule with matching OwnerID | Update IP, merge headers/portmap |
| 3. Header takeover | Different owner, same headers | Replace IP and OwnerID (takeover) |
| 4. New user | Different owner, different headers | Append new Rule |

After merging, rules are sorted so that rules with headers come first — envoy evaluates routes in order, so a catch-all (empty headers) must be last.

### Removing Rules

`removeEnvoyConfig(ctx, configMapInterface, namespace, nodeID, ownerID)` delegates to
`VirtualStore.RemoveRule`, which performs the same RetryOnConflict Get+Update loop:
1. Find Virtual by nodeID + namespace
2. Remove all Rules matching the ownerID
3. If Virtual has no remaining rules, remove the entire Virtual entry
4. Returns `(empty, found)` — `empty=true` means containers should be unpatched

### Parameter Object

`envoyRuleSpec` bundles all parameters to avoid 10+ function arguments:

```go
type envoyRuleSpec struct {
    Namespace, NodeID        string
    LocalTunIPv4, LocalTunIPv6 string
    Headers                  map[string]string
    Ports                    []controlplane.ContainerPort
    PortMap                  map[int32]string
    FargateMode              bool
    OwnerID                  string
}
```

## 6. Workload Patching

`patchWorkload(ctx, factory, info, templateSpec, path)`:
- **Controller-managed** (Deployment, StatefulSet, etc.): JSON Patch to `spec.template.spec`, then `RolloutStatus` wait
- **Bare Pod** (no controller): Delete + recreate with retry

`UnpatchContainer(ctx, nodeID, factory, configMapInterface, object, ownerID)`:
1. Remove envoy config rules for this ownerID
2. If no rules remain (empty), call `RemoveContainers` and patch workload
3. If other users still have rules, leave containers in place

## 7. Embedded Envoy Config Templates

Four embedded YAML templates for envoy bootstrap config. All templates share the same structure: admin endpoint + `dynamic_resources` (ADS for LDS/CDS/RDS/EDS) + `static_resources` (only the `xds_cluster` for connecting to the control plane). There are no static listeners — all listeners are dynamically created by the xDS control plane.

| Template | Mode | IP Version |
|----------|------|-----------|
| `envoy.yaml` | Mesh | IPv6 (dual-stack) |
| `envoy_ipv4.yaml` | Mesh | IPv4 only |
| `fargate_envoy.yaml` | Fargate | IPv6 (dual-stack) |
| `fargate_envoy_ipv4.yaml` | Fargate | IPv4 only |

The only difference between mesh and fargate templates is that listeners generated by the control plane use `BindToPort=true` in fargate mode (envoy opens a real socket) vs `BindToPort=false` in mesh mode (iptables handles initial packet capture). Templates use Go `text/template` with the traffic manager address as the template value.

## 8. Related Files

| File | Purpose |
|------|---------|
| `pkg/inject/injector.go` | Injector interface, factory, shared helpers |
| `pkg/inject/mesh.go` | Mesh (VPN+Envoy) strategy + UnpatchContainer — also serves VPN-only (empty headers) |
| `pkg/inject/fargate.go` | Fargate (SSH+Envoy) strategy |
| `pkg/inject/container.go` | Container spec builders |
| `pkg/inject/envoy.go` | ConfigMap envoy rule CRUD (thin wrappers over VirtualStore) |
| `pkg/inject/virtual_store.go` | `VirtualStore` — unified read-modify-write path for `ENVOY_CONFIG` |
| `pkg/xds/cache.go` | Virtual/Rule types consumed by inject |
| `docs/06-fargate-mode.md` | Fargate mode architecture |
| `docs/05-owner-id.md` | OwnerID rule ownership |
