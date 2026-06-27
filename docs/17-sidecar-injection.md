# Sidecar Injection Design

## 1. Overview

The inject package (`pkg/inject`) manages the lifecycle of sidecar containers injected into Kubernetes workloads. It uses the **Strategy pattern** to choose between three injection modes based on the target workload and user options. It also manages envoy routing rules in the traffic manager ConfigMap.

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
| Headers or port maps specified | `meshInjector` | VPN + Envoy |
| Otherwise | `vpnInjector` | VPN only |

All three strategies share the same `InjectOptions` parameter object:

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

### 3.1 VPN-Only (`vpnInjector`)

Used for `kubevpn proxy` without header-based traffic splitting.

```
Inject:
  1. collectPorts → envoy ports + portmap
  2. addEnvoyConfig → write Virtual/Rule to ConfigMap
  3. AddVPNContainer → inject VPN sidecar
  4. patchWorkload → JSON patch the controller
```

The VPN container:
- Enables IP forwarding and iptables DNAT rules
- All non-ICMP traffic is DNATed to the user's TUN IP
- Runs `kubevpn server` with TUN tunnel to traffic manager
- Requires `NET_ADMIN` capability + privileged mode

### 3.2 Mesh (`meshInjector`)

Used for `kubevpn proxy` with `--headers` for traffic splitting.

```
Inject:
  1. collectPorts → envoy ports + portmap
  2. addEnvoyConfig → write Virtual/Rule to ConfigMap
  3. Check if already injected (skip container add)
  4. AddVPNAndEnvoyContainer → inject VPN + Envoy sidecars
  5. patchWorkload → JSON patch the controller
```

The mesh mode adds two containers:
- **VPN container**: Same as VPN-only but iptables DNAT to port 15006 (envoy inbound) instead of TUN IP directly
- **Envoy container**: Runs envoy with xDS config pointing to the control plane, routes by headers

Multiple users can proxy the same workload with different headers — envoy routes each user's traffic to their TUN IP while unmatched traffic goes to `origin_cluster` (the real application).

### 3.3 Fargate/Service (`fargateInjector`)

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
| `AddVPNContainer` | VPN only | Privileged + NET_ADMIN |
| `AddVPNAndEnvoyContainer` | VPN + Envoy | VPN: privileged; Envoy: unprivileged |
| `AddEnvoyAndSSHContainer` | SSH + Envoy | Both unprivileged |

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

Four embedded YAML templates for envoy bootstrap config:

| Template | Mode | IP Version |
|----------|------|-----------|
| `envoy.yaml` | Mesh (iptables redirect) | IPv6 |
| `envoy_ipv4.yaml` | Mesh | IPv4 only |
| `fargate_envoy.yaml` | Fargate (bind to port) | IPv6 |
| `fargate_envoy_ipv4.yaml` | Fargate | IPv4 only |

Templates use Go `text/template` with the traffic manager address as the template value. The envoy connects to the control plane via the `xds_cluster` configured in the bootstrap.

## 8. Related Files

| File | Purpose |
|------|---------|
| `pkg/inject/injector.go` | Injector interface, factory, shared helpers |
| `pkg/inject/vpn.go` | VPN-only strategy |
| `pkg/inject/mesh.go` | Mesh (VPN+Envoy) strategy + UnpatchContainer |
| `pkg/inject/fargate.go` | Fargate (SSH+Envoy) strategy |
| `pkg/inject/container.go` | Container spec builders |
| `pkg/inject/envoy.go` | ConfigMap envoy rule CRUD |
| `pkg/controlplane/cache.go` | Virtual/Rule types consumed by inject |
| `docs/06-fargate-mode.md` | Fargate mode architecture |
| `docs/05-owner-id.md` | OwnerID rule ownership |
