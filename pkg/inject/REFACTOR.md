# pkg/inject Refactoring Design

## Current Design

The `inject` package manages sidecar container injection into Kubernetes workloads. Three injection modes exist:

1. **VPN-only** (`InjectVPN`) — Adds a VPN sidecar with iptables DNAT rules. Used when proxying a workload with no header routing or port remapping.
2. **Mesh / Envoy+VPN** (`InjectEnvoyAndVPN`) — Adds both VPN and Envoy sidecars. Used when header-based routing or port maps are specified.
3. **Fargate / Envoy+SSH** (`InjectEnvoyAndSSH`) — Adds SSH and Envoy sidecars. Used when the target is a Kubernetes Service (Fargate-like environments without NET_ADMIN).

### Data Flow

```
handler/connect.go
  ├─ if IsK8sService(object)     → inject.InjectEnvoyAndSSH(...)
  ├─ elif headers or portMaps    → inject.InjectEnvoyAndVPN(...)
  └─ else                       → inject.InjectVPN(...)

handler/leave.go
  └─ inject.UnPatchContainer(...)   // mode-agnostic removal via callback

handler/reset.go
  └─ inject.RemoveContainers(...)   // strip sidecars
  └─ inject.P{...}                  // JSON patch struct
  └─ inject.ModifyServiceTargetPort(...)

run/runconfig.go
  └─ inject.RemoveContainers(...)   // strip sidecars before local run
```

### File layout (before)

| File | Responsibility | Lines |
|------|----------------|-------|
| `proxy.go` | `InjectVPN`, `CreateAfterDeletePod`, `CleanupUselessInfo`, `P` struct | 140 |
| `controller.go` | Container builders (`AddVPNContainer`, `AddVPNAndEnvoyContainer`, `AddEnvoyAndSSHContainer`), `RemoveContainers`, envoy embed + `GetEnvoyConfig` | 355 |
| `mesh.go` | `InjectEnvoyAndVPN`, `UnPatchContainer`, envoy ConfigMap CRUD (`addEnvoyConfig`, `removeEnvoyConfig`, `addVirtualRule`) | 333 |
| `fargate.go` | `InjectEnvoyAndSSH`, `ModifyServiceTargetPort`, `GetPort` | 166 |

## Problem

1. **No abstraction over modes** — The caller (`connect.go`) must know about all three injection functions and their different parameter signatures. Adding a fourth mode would require changing the caller.
2. **Heavy code duplication** — All three `Inject*` functions repeat: get PodTemplateSpec, collect ports, configure envoy, patch workload, wait for rollout.
3. **Misleading file names** — `proxy.go` contains VPN injection, `controller.go` contains container builders, not controller logic.
4. **Mixed concerns** — `controller.go` mixes container building with envoy template rendering. `mesh.go` mixes high-level injection orchestration with low-level ConfigMap manipulation.
5. **Poorly named exports** — `P` struct is just a JSON Patch operation. `GetEnvoyConfig` renders a template, not "gets" a config.

## New Design

### Interface

```go
type Injector interface {
    Inject(ctx context.Context) error
}

func NewInjector(opts InjectOptions) Injector  // factory
```

Three concrete implementations: `vpnInjector`, `meshInjector`, `fargateInjector`.

### Dependency Diagram

```
handler/connect.go
  └─ inject.NewInjector(opts) → Injector.Inject(ctx)

inject/
  injector.go     ← interface, InjectOptions, factory, shared helpers (patch, rollout, pod lifecycle)
  vpn.go          ← vpnInjector: VPN-only mode
  mesh.go         ← meshInjector: Envoy+VPN mode, UnPatchContainer (removal)
  fargate.go      ← fargateInjector: Envoy+SSH mode, ModifyServiceTargetPort, GetPort
  container.go    ← container builders (AddVPNContainer, AddVPNAndEnvoyContainer, AddEnvoyAndSSHContainer, RemoveContainers)
  envoy.go        ← envoy ConfigMap CRUD, virtual rule logic, template rendering, embedded YAML
```

### Key Renames

| Before | After |
|--------|-------|
| `P` | `JSONPatchOp` |
| `GetEnvoyConfig` | `RenderEnvoyConfig` |
| `controller.go` | `container.go` |
| `proxy.go` | split into `injector.go` + `vpn.go` |

## Migration Strategy

Pure internal refactoring — no external API changes. The `inject` package is only consumed by `handler/` and `run/`. All callers updated in the same commit.

Backward compatibility: `UnPatchContainer`, `RemoveContainers`, `ModifyServiceTargetPort`, `CleanupUselessInfo`, `CreateAfterDeletePod`, `GetPort` remain exported with the same signatures. `JSONPatchOp` replaces `P` (only used in `handler/reset.go`).

## Files Changed

### New files
- `pkg/inject/injector.go` — interface, options, factory, shared helpers
- `pkg/inject/envoy.go` — envoy ConfigMap CRUD + template rendering
- `pkg/inject/vpn.go` — VPN-only injector

### Renamed
- `pkg/inject/controller.go` → `pkg/inject/container.go`

### Deleted
- `pkg/inject/proxy.go` (content moved to `injector.go` + `vpn.go`)

### Modified
- `pkg/inject/mesh.go` — rewritten as meshInjector implementation
- `pkg/inject/fargate.go` — rewritten as fargateInjector implementation
- `pkg/inject/container.go` — removed duplicated embeds/render, use `RenderEnvoyConfig`
- `pkg/inject/render_test.go` — updated to use `RenderEnvoyConfig`
- `pkg/handler/connect.go` — use `NewInjector` factory instead of 3-way if-else
- `pkg/handler/reset.go` — `inject.P` → `inject.JSONPatchOp`

---

## Follow-up: Eliminate `util.PodRouteConfig`

### Problem

`PodRouteConfig` in `pkg/util/pod.go` was a two-field struct:

```go
type PodRouteConfig struct {
    LocalTunIPv4 string
    LocalTunIPv6 string
}
```

Issues:
1. **Unnecessary wrapper** — Only two string fields, always decomposed immediately at use sites. Never passed around as a unit or stored.
2. **Misleading name** — "PodRouteConfig" implies routing configuration, but it only carries a pair of TUN IP addresses.
3. **Wrong package** — Defined in `pkg/util` (grab-bag), consumed exclusively by `pkg/inject`.

### Solution

Eliminated the struct entirely. Replaced all uses with plain `localTunIPv4, localTunIPv6 string` parameters:

- `InjectOptions.RouteConfig` → `InjectOptions.LocalTunIPv4` + `InjectOptions.LocalTunIPv6`
- `addEnvoyConfig(... tunIP PodRouteConfig ...)` → `addEnvoyConfig(... localTunIPv4, localTunIPv6 string ...)`
- `addVirtualRule(... tunIP PodRouteConfig ...)` → `addVirtualRule(... localTunIPv4, localTunIPv6 string ...)`
- `AddVPNContainer(... c PodRouteConfig ...)` → `AddVPNContainer(... localTunIPv4, localTunIPv6 string ...)`

### Files changed
- `pkg/util/pod.go` — removed `PodRouteConfig` struct
- `pkg/inject/injector.go` — inlined fields into `InjectOptions`
- `pkg/inject/envoy.go` — plain string params
- `pkg/inject/vpn.go` — use `o.LocalTunIPv4`, `o.LocalTunIPv6`
- `pkg/inject/mesh.go` — use `o.LocalTunIPv4`, `o.LocalTunIPv6`
- `pkg/inject/fargate.go` — local variables instead of struct literal
- `pkg/inject/container.go` — `AddVPNContainer` takes two strings
- `pkg/inject/envoyrule_test.go` — updated test data struct
- `pkg/handler/connect.go` — removed `configInfo` struct, use plain strings
