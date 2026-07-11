# Fargate Mode Design Document

## Overview

Fargate mode is one of three traffic interception strategies in KubeVPN, designed for environments that **lack privileged container capabilities** (no `NET_ADMIN`, no `privileged: true`). The canonical example is AWS Fargate, hence the name. It is also known as "Service mode" because it is triggered when the user proxies a **Kubernetes Service** (`svc/...`) rather than a workload directly.

### Strategy Comparison

| | VPN Mode | Mesh Mode | Fargate Mode |
|---|---|---|---|
| **Target** | Deployment, StatefulSet, etc. | Workload with port mapping | `svc/<name>` |
| **Requires privileged** | Yes | Yes | **No** |
| **Uses iptables** | Yes (DNAT to TUN) | Yes (DNAT to envoy:15006) | **No** |
| **Sidecar containers** | VPN only | VPN + Envoy | **SSH + Envoy** |
| **Traffic interception** | Kernel-level (TUN device) | iptables PREROUTING + envoy `use_original_dst` | envoy `BindToPort=true` + SSH reverse tunnel |
| **User identity** | TUN IP (`198.18.x.x`) | TUN IP | HTTP headers |
| **Injector** | `vpnInjector` | `meshInjector` | `fargateInjector` |

## Architecture

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ Kubernetes Cluster                                                            │
│                                                                               │
│  External Request                                                             │
│       │                                                                       │
│       ▼                                                                       │
│  ┌─────────────────────────────────────┐                                     │
│  │ K8s Service (svc/reviews)           │                                     │
│  │ ports:                              │                                     │
│  │   - port: 9080                      │                                     │
│  │     targetPort: 38721  ◀── modified │                                     │
│  └──────────────┬──────────────────────┘                                     │
│                 │                                                              │
│                 ▼                                                              │
│  ┌────────────────────────────────────────────────────────────────────┐      │
│  │ Pod (reviews-xxx)                                                   │      │
│  │                                                                     │      │
│  │  ┌─────────────────────────────────────────────────────────┐      │      │
│  │  │ envoy-proxy (sidecar)                                    │      │      │
│  │  │                                                          │      │      │
│  │  │  listen :38721 (BindToPort=true)                         │      │      │
│  │  │       │                                                  │      │      │
│  │  │       ├─ header "env: test" matched                      │      │      │
│  │  │       │     → route to 127.0.0.1:29450 (envoyRulePort)   │      │      │
│  │  │       │                                                  │      │      │
│  │  │       └─ no match (default route)                        │      │      │
│  │  │             → route to 127.0.0.1:9080 (containerPort)    │      │      │
│  │  └─────────────────────────────────────────────────────────┘      │      │
│  │                                                                     │      │
│  │  ┌─────────────────────────────────────────────────────────┐      │      │
│  │  │ vpn (sidecar) — SSH server on :2222                      │      │      │
│  │  │   reverse tunnel: pod:29450 → local:19080                │      │      │
│  │  └─────────────────────────────────────────────────────────┘      │      │
│  │                                                                     │      │
│  │  ┌─────────────────────────────────────────────────────────┐      │      │
│  │  │ app (original container)                                 │      │      │
│  │  │   listening on :9080 (handles unmatched traffic)         │      │      │
│  │  └─────────────────────────────────────────────────────────┘      │      │
│  └────────────────────────────────────────────────────────────────────┘      │
│                                                                               │
└──────────────────────────────────────────────────────────────────────────────┘

                    SSH reverse tunnel
                         │
                         ▼
┌──────────────────────────────────────────────────────────────────────────────┐
│ Local Machine                                                                 │
│                                                                               │
│  Developer's app listening on :19080                                          │
│  (receives only requests with header "env: test")                             │
│                                                                               │
└──────────────────────────────────────────────────────────────────────────────┘
```

## Port Naming Convention

Fargate mode involves three distinct port numbers per container port:

| Port | Name | Example | Where it lives |
|---|---|---|---|
| Container port | `containerPort` | 9080 | Original workload spec |
| Envoy listener port | `EnvoyListenerPort` | 38721 | Random; set in `fargateInjector.Inject()`, stored in `ContainerPort.EnvoyListenerPort`; used as Service `targetPort` |
| Envoy rule port | `envoyRulePort` | 29450 | Random; stored in `Rule.PortMap` as `containerPort -> "envoyRulePort:localPort"`; where envoy routes matched traffic |
| Local port | `localPort` | 19080 | Random; stored as second part of `PortMap` value; the SSH tunnel target on the local machine |

The mapping chain:
```
Service targetPort (38721)
  → envoy listener (BindToPort=true on 38721)
    → header match → envoy route to 127.0.0.1:29450 (envoyRulePort)
      → SSH reverse tunnel → local machine:19080 (localPort)
    → no match → route to 127.0.0.1:9080 (containerPort, original app)
```

## Key Data Structures

### `Virtual` (`pkg/controlplane/cache.go`)

```go
type Virtual struct {
    Namespace   string
    UID         string            // e.g., "deployments.apps.productpage"
    FargateMode bool              // explicit mode flag — true for Service/Fargate targets
    Ports       []ContainerPort
    Rules       []*Rule
}
```

The `Virtual` struct is serialized to YAML in the `kubevpn-traffic-manager` ConfigMap under the `ENVOY_CONFIG` key. The control plane watches this ConfigMap and generates xDS snapshots from it.

**`FargateMode`** is the **explicit discriminator** — set to `true` by `fargateInjector` when writing the ConfigMap. The `IsFargateMode()` method checks this field first, with a legacy fallback to `EnvoyListenerPort != 0` for backward compatibility with existing ConfigMaps that predate the explicit field.

### `ContainerPort` (`pkg/controlplane/cache.go`)

```go
type ContainerPort struct {
    Name              string
    EnvoyListenerPort int32  // port envoy binds to in fargate mode; 0 in mesh mode
    ContainerPort     int32
    Protocol          corev1.Protocol
}
```

`EnvoyListenerPort` holds the actual envoy listening port number in fargate mode. It is no longer the mode discriminator — that role belongs to `Virtual.FargateMode`.

### `Rule` (`pkg/controlplane/cache.go`)

```go
type Rule struct {
    Headers      map[string]string         // header-based routing rules
    LocalTunIPv4 string                    // "127.0.0.1" (fargate) or real TUN IP (mesh)
    LocalTunIPv6 string                    // "::1" (fargate) or real TUN IPv6 (mesh)
    PortMap      map[int32]string          // containerPort -> "envoyRulePort:localPort"
}
```

In fargate mode, `LocalTunIPv4` is always `"127.0.0.1"` because there's no TUN device — traffic flows through SSH tunnels to localhost.

## Control Plane Behavior

### Envoy Bootstrap

Fargate mode uses dedicated bootstrap templates (`fargate_envoy.yaml`, `fargate_envoy_ipv4.yaml`) that differ from mesh mode in one critical way:

- **Mesh**: Static listener on port 15006 with `use_original_dst: true` — catches iptables-redirected traffic
- **Fargate**: **No static listener** — all listeners are dynamically created by the xDS control plane

Both share the same xDS cluster configuration pointing to `<traffic-manager>:9002`.

### xDS Listener Generation (`Virtual.To()`)

The `To()` method generates envoy xDS resources. Fargate mode affects:

1. **Listener port**: Uses `EnvoyListenerPort` (the random port) instead of `ContainerPort`
2. **BindToPort**: Set to `true` — envoy opens a real socket. In mesh mode, this is `false` because iptables handles the initial packet capture.
3. **Default route**: Routes unmatched traffic to `127.0.0.1:containerPort` (the original app). Mesh mode uses `ORIGINAL_DST` cluster for this, which doesn't work without iptables.
4. **Endpoint address**: Uses `envoyRulePort` (from `PortMap`) instead of the raw container port.

## SSH Reverse Tunnels

### Mapper (`pkg/handler/proxy.go`)

The `Mapper` struct manages SSH tunnels for fargate-mode proxied resources. Created only when `util.IsK8sService(object)` returns true.

```
Mapper.Run():
  watches (via shared ConfigMap informer + pod informer):
    1. getPortMappingFromCache() — read port map from shared informer cache
    2. reconcilePodsFromInformer() — react to pod add/update/delete events
    3. For each pod, startTunnels():
       - For each portMap entry: envoyRulePort → localPort
       - ssh.ExposeLocalPortToRemote(podIP:2222, envoyRulePort, localPort)
  fallback: 30s ticker reconciliation
```

### SSH Server

The VPN sidecar in fargate mode runs `kubevpn server -l ssh://:2222` — a no-auth SSH server. Security relies on the pod network boundary.

### Tunnel Data Flow

```
Pod envoy → 127.0.0.1:envoyRulePort
  → SSH server (:2222) accepts tunnel connection
  → SSH client on local machine dials localhost:localPort
  → Developer's app receives the request
```

## Injection Flow

```
kubevpn proxy svc/reviews --headers env=test
  │
  ├── 1. NewInjector detects IsK8sService → fargateInjector
  │
  ├── 2. collectFargatePorts(): collect container ports, assign random envoyRulePort + localPort per port
  │
  ├── 3. Assign random EnvoyListenerPort for each ContainerPort
  │
  ├── 4. addEnvoyConfig(): write Virtual to ConfigMap
  │      LocalTunIPv4 = "127.0.0.1"  (not real TUN IP)
  │      PortMap[containerPort] = "envoyRulePort:localPort"
  │
  ├── 5. AddEnvoyAndSSHContainer(): inject sidecars
  │      vpn container:   kubevpn server -l ssh://:2222
  │      envoy container: fargate bootstrap (no static listener)
  │
  ├── 6. ModifyServiceTargetPort(): svc.targetPort = EnvoyListenerPort
  │
  └── 7. Start Mapper goroutine → watch pods → establish SSH tunnels
```

## Cleanup Flow

```
kubevpn leave svc/reviews
  │
  ├── 1. Rule matching: use headers (not TUN IP) to find user's rule
  │      isFargateMode detected via EnvoyListenerPort != 0
  │
  ├── 2. Remove rule from ConfigMap
  │      If last rule → remove all sidecar containers from workload
  │
  ├── 3. Restore Service targetPort to original containerPort values
  │
  └── 4. Stop Mapper → tear down SSH tunnels
```

## Fargate Mode Detection

Fargate mode is indicated by the explicit `Virtual.FargateMode` bool field, set to `true` by `fargateInjector` when writing the ConfigMap entry. All consumers call `virtual.IsFargateMode()` which checks the field first, with a legacy fallback to the `EnvoyListenerPort != 0` heuristic for backward compatibility.

Consumers:
- `pkg/controlplane/cache.go` — xDS generation (`Virtual.To()`)
- `pkg/inject/envoy.go` — envoy config creation and removal
- `pkg/daemon/action/status.go` — status reporting
- `pkg/handler/leave.go` — cleanup rule matching

## Limitations and Constraints

### Mode Mixing

Fargate mode **can** coexist with other modes in the same connection session — e.g., `kubevpn proxy svc/reviews` (Fargate) alongside `kubevpn proxy deployment/ratings` (Mesh). They write separate `Virtual` entries in the same ConfigMap and do not interfere.

However, within a single workload the mode is fixed: once a Service is injected with Fargate sidecars (`FargateMode=true`, `targetPort` modified, SSH server running), all subsequent users proxying that Service also go through Fargate. This is correct — the container already has SSH instead of VPN.

### Service-Only

Fargate mode is **only triggered for Service targets** (`svc/xxx`). `NewInjector` checks `util.IsK8sService(opts.Object)` as the sole condition. You cannot use Fargate mode on a Deployment even if the cluster lacks privileged container support.

### User Identity via Headers Only

In both Mesh/VPN and Fargate modes, rule ownership is now identified by `OwnerID` (a per-connection UUID). In Fargate mode, `LocalTunIPv4` is always `127.0.0.1` — but this no longer matters for ownership matching since `OwnerID` is used instead:

- Multiple users **must** use different `--headers` values (for envoy routing)
- If two users proxy the same Service with identical headers, the second overwrites the first (`addVirtualRule` case 3)
- `leave` and `removeEnvoyConfig` match by `OwnerID`, not TUN IP or Headers

### TCP Only (No UDP/ICMP)

The Fargate data path is: envoy (TCP/HTTP only) → SSH reverse tunnel (TCP only) → local app. There is no TUN device, so arbitrary IP-layer protocols (UDP, ICMP) are not supported.

### Proxy Only (No Connect)

Fargate mode does not create a TUN device or establish a VPN tunnel. It only affects `kubevpn proxy svc/xxx` — the traffic interception mechanism. `kubevpn connect` for cluster network access is independent and always uses the VPN/TUN path.

### SSH Server Security

The SSH server runs on `:2222` without authentication. Any process that can reach the Pod IP can connect. Security relies entirely on Kubernetes network policies and pod network isolation.

### Service targetPort Modification

Fargate modifies the Service's `targetPort` from the original container port to a random envoy listener port. If kubevpn exits abnormally without cleanup, the Service will point to a nonexistent port. Recovery requires `kubevpn reset svc/xxx` to restore the original targetPort values.

### Random Port Allocation Window

Fargate generates two random ports per container port (`EnvoyListenerPort` + `envoyRulePort`) using `GetAvailableTCPPort`. There is a TOCTOU window between port allocation and container startup where the port could be claimed by another process. In practice this is rare.

## Source Files

| File | Fargate-related content |
|---|---|
| `pkg/inject/injector.go` | `NewInjector` factory: Service → fargateInjector |
| `pkg/inject/fargate.go` | `fargateInjector.Inject()`, `collectFargatePorts()`, `ModifyServiceTargetPort()` |
| `pkg/inject/container.go` | `AddEnvoyAndSSHContainer()` — SSH + Envoy sidecars |
| `pkg/inject/envoy.go` | `addEnvoyConfig()`, `removeEnvoyConfig()` with fargate detection |
| `pkg/inject/fargate_envoy.yaml` | Envoy bootstrap template (no static listener) |
| `pkg/inject/fargate_envoy_ipv4.yaml` | Envoy bootstrap template IPv4 variant |
| `pkg/controlplane/cache.go` | `Virtual.To()` — xDS generation with `BindToPort`, default routes |
| `pkg/handler/proxy.go` | `Mapper` — SSH tunnel lifecycle management |
| `pkg/handler/leave.go` | Cleanup: header-based rule matching, Service restoration |
| `pkg/handler/reset.go` | Hard reset: remove all rules, restore Service |
| `pkg/ssh/reverse.go` | `ExposeLocalPortToRemote()` — SSH reverse tunnel |
| `pkg/daemon/action/status.go` | Status reporting: fargate-aware device detection |
