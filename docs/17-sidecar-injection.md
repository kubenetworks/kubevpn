# Sidecar Injection Design

## 1. Overview

The inject package (`pkg/inject`) manages the lifecycle of sidecar containers injected into Kubernetes workloads. It uses the **Strategy pattern** to choose between two injection modes based on the target workload. It also manages envoy routing rules in the traffic manager ConfigMap.

> **Unified proxy mode:** there is no longer a separate VPN-only injector. VPN-only proxying (`kubevpn proxy` without `--headers`) is just Mesh mode with **empty headers** — envoy matches all requests and forwards everything to the user's TUN IP. This eliminates the old hardcoded iptables-DNAT-to-client-IP path and lets all routing (TCP + UDP) hot-update via envoy xDS. See [28-sleep-wake-ip-update.md](28-sleep-wake-ip-update.md).

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
  3. Check if already injected (skip container add)
  4. AddVPNAndEnvoyContainer → inject VPN + Envoy sidecars
  5. patchWorkload → JSON patch the controller
```

Mesh mode adds two containers:
- **VPN container**: enables IP forwarding and DNATs all non-ICMP, non-cluster traffic to envoy's inbound port `:15006` (a *fixed* port — no client-IP in the rule). Runs `kubevpn server -l "tun:/tcp://...:10801?route=${CIDR4}"` with `net=` empty, so it self-allocates its TUN IP from the control-plane at startup. Requires `NET_ADMIN` + `NET_RAW` capabilities (runs as root but not privileged).
- **Envoy container**: runs envoy with xDS config pointing to the control plane; routes by header.

**Routing behavior depends on whether headers are set:**

| Headers | envoy route | Effect |
|---------|-------------|--------|
| Empty (VPN-only) | `RouteMatch{Prefix:"/"}` matches everything | All traffic forwarded to the user's TUN IP (full interception) |
| Set (`--headers`) | header match → user IP; no match → `origin_cluster` | Per-user traffic splitting; unmatched requests hit the real app |

Multiple users can proxy the same workload with different headers — envoy routes each user's traffic to their own TUN IP. Because the routing target (`Rule.LocalTunIPv4/v6`) lives in `ENVOY_CONFIG`, a client IP change is picked up automatically via `syncEnvoyRuleIP` → xDS push (TCP + UDP), with no Pod restart.

### 3.2 Fargate/Service (`fargateInjector`)

Used when targeting a K8s Service (no `NET_ADMIN` capability available).

```
Inject:
  1. collectFargatePorts → allocate random envoy listener ports
  2. addEnvoyConfig → write Virtual/Rule with FargateMode=true
  3. Check if already injected
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

## 4. Container Builders (`container.go`)

### Environment Variables

All sidecar containers receive:

| Env Var | Source | Purpose |
|---------|--------|---------|
| `CIDR4`, `CIDR6` | `config.CIDR` | VPN address pool |
| `TrafficManagerService` | `kubevpn-traffic-manager.{ns}` | gRPC endpoint |
| `POD_NAMESPACE` | Downward API | Current namespace |
| `POD_NAME` | Downward API | Current pod name |
| `tls_crt`, `tls_key`, `tls_server_name` | TLS Secret | mTLS credentials (VPN/mesh only) |

### Container Types

| Function | Containers | Security |
|----------|-----------|----------|
| `AddVPNAndEnvoyContainer` | VPN + Envoy (all non-Service proxy) | VPN: NET_ADMIN + NET_RAW (not privileged); Envoy: unprivileged |
| `AddEnvoyAndSSHContainer` | SSH + Envoy (Service/Fargate) | Both unprivileged |

> The former `AddVPNContainer` (VPN-only, no envoy) was **removed** with the unified proxy mode — every non-Service proxy now goes through `AddVPNAndEnvoyContainer`.

## 5. Envoy ConfigMap Management (`envoy.go`)

### Writing Rules

`addEnvoyConfig(ctx, configMapInterface, envoyRuleSpec)`:
1. GET ConfigMap with `RetryOnConflict`
2. Unmarshal YAML from `ENVOY_CONFIG` key → `[]*Virtual`
3. Call `addVirtualRule(virtuals, spec)` to merge
4. Marshal back to YAML and UPDATE ConfigMap

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

`removeEnvoyConfig(ctx, configMapInterface, namespace, nodeID, ownerID)`:
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
| `pkg/inject/envoy.go` | ConfigMap envoy rule CRUD |
| `pkg/controlplane/cache.go` | Virtual/Rule types consumed by inject |
| `docs/06-fargate-mode.md` | Fargate mode architecture |
| `docs/05-owner-id.md` | OwnerID rule ownership |
