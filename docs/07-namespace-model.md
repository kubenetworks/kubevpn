# Namespace Model: Manager vs Workload

## Overview

KubeVPN operates across two namespaces that may or may not be the same. Confusing them is a common source of bugs, so this document clarifies exactly which namespace each component and data structure refers to.

## The Two Namespaces

### Manager Namespace

**What:** The namespace where the traffic manager infrastructure lives.

**Contains:**
- `kubevpn-traffic-manager` Deployment (vpn + control-plane containers)
- `kubevpn-traffic-manager` Service (ports 10801, 9002, 53)
- `kubevpn-traffic-manager` ConfigMap (DHCP leases + envoy config)
- `kubevpn-traffic-manager` Secret (TLS certificates)
- ServiceAccount, Role, RoleBinding


**How determined:**
1. User specifies `--manager-namespace` explicitly, OR
2. `DetectManagerNamespace()` auto-detects by searching:
   - The user's `-n` namespace (check if traffic manager pod exists)
   - The default namespace `kubevpn` (check if traffic manager pod exists)
   - Any namespace with a deployed Helm release named `kubevpn`

**In code:** `ConnectOptions.ManagerNamespace` (the primary `Namespace` field)

### Workload Namespace

**What:** The namespace where the user's application runs — the target of `kubevpn proxy` or `kubevpn connect`.

**Contains:**
- The user's Deployments, StatefulSets, Services, Pods
- Sidecar containers injected by kubevpn (vpn/envoy/ssh)

**How determined:** User specifies `-n <namespace>` or kubeconfig default namespace.

**In code:** `ConnectOptions.WorkloadNamespace`, and the `namespace` parameter in `CreateRemoteInboundPod()`

## When They Differ

```
# Cluster-wide mode: traffic manager in dedicated namespace
kubevpn connect -n default --manager-namespace kubevpn

  Manager Namespace = "kubevpn"      → traffic manager lives here
  Workload Namespace = "default"     → user's apps live here
```

```
# Same-namespace mode (default): everything in one namespace
kubevpn connect -n default

  Manager Namespace = "default"      → detected: traffic manager created here
  Workload Namespace = "default"     → user's apps live here too
```

## Field Mapping

### ConnectOptions

```go
type ConnectOptions struct {
    ManagerNamespace      string  // ← MANAGER namespace (traffic manager infrastructure)
    WorkloadNamespace     string  // ← WORKLOAD namespace (user's -n flag)
    OriginKubeconfigPath  string  // ← user's kubeconfig path
    // ...
}
```

### ConnectRequest (RPC)

```protobuf
message ConnectRequest {
    string KubeconfigBytes = 1;
    string Namespace = 2;           // ← WORKLOAD namespace (user's -n flag)
    // ... other fields ...
    string ManagerNamespace = 12;   // ← MANAGER namespace
}
```

**Mapping in daemon/action/connect.go:**
```go
connect := &handler.ConnectOptions{
    ManagerNamespace: req.ManagerNamespace,   // same name, clear intent
    WorkloadNamespace:  req.Namespace,        // req.Namespace → WorkloadNamespace
}
```

### InjectOptions

```go
type InjectOptions struct {
    ManagerNamespace string  // ← MANAGER namespace (for ConfigMap, envoy config)
    // Object and Controller carry their own namespace (WORKLOAD namespace)
}
```

### Mapper (Fargate SSH tunnels)

```go
type Mapper struct {
    ns         string              // ← WORKLOAD namespace (pods live here)
    cmInformer cache.SharedInformer // ← watches MANAGER namespace ConfigMap
}
```

## Which Namespace for What

### Manager Namespace (`c.ManagerNamespace`)

| Resource | API call |
|----------|----------|
| ConfigMap CRUD | `ConfigMaps(c.ManagerNamespace).Get/Update/Patch` |
| Secret (TLS) | `Secrets(c.ManagerNamespace).Get` |
| Deployment (traffic manager) | `Deployments(c.ManagerNamespace).Get/Create` |
| Service (traffic manager) | `Services(c.ManagerNamespace).Get/Create` |
| TunConfigService | gRPC GetTunIP/WatchTunIP (server-side DHCP) |
| ConfigMap informer | `GetConfigMapInformer()` → `c.ManagerNamespace` |
| Pod list (traffic manager) | `GetRunningPodList(c.ManagerNamespace)` |
| CIDR detection | `GetCIDR(c.ManagerNamespace)` |
| Port-forward target | `PortForwardPod(c.ManagerNamespace)` |
| Envoy config read/write | `addEnvoyConfig(ConfigMaps(o.ManagerNamespace))` |

### Workload Namespace (`namespace` param / `m.ns` / `c.WorkloadNamespace`)

| Resource | API call |
|----------|----------|
| Sidecar injection | `patchWorkload(factory, controller)` (controller carries its own ns) |
| Pod informer (Mapper) | `NewFilteredPodInformer(m.ns)` |
| Service targetPort mod | `Services(namespace).Get/Update` |
| Leave/unpatch | `GetTopOwnerObject(factory, workload.Namespace)` |
| DNS service lookup | `Services(c.WorkloadNamespace).List` |
| Route table (pods) | `Pods(c.WorkloadNamespace).List` |
| Status reporting | `connect.WorkloadNamespace` shown to user |

## Detection Flow

```
kubevpn connect -n default --manager-namespace kubevpn
                    │                    │
                    ▼                    ▼
           req.Namespace          req.ManagerNamespace
           = "default"            = "kubevpn"
                    │                    │
                    ▼                    ▼
        connect.WorkloadNamespace   connect.ManagerNamespace
        = "default"               = "kubevpn"
```

```
kubevpn connect -n default    (no --manager-namespace)
                    │
                    ▼
           req.Namespace = "default"
           req.ManagerNamespace = ""
                    │
                    ▼
         DetectManagerNamespace("default")
           1. Check "default" → traffic manager pod found? → use "default"
           2. Check "kubevpn" → traffic manager pod found? → use "kubevpn"
           3. Check Helm releases → found? → use release namespace
           4. Not found → create in "default" (req.ManagerNamespace = req.Namespace)
                    │
                    ▼
        connect.WorkloadNamespace = "default"
        connect.ManagerNamespace = detected result
```

Detection runs in the **User Daemon only** (`detectAndSetManagerNamespace`). The resolved
namespace is written to both `connect.ManagerNamespace` (User Daemon) and `req.ManagerNamespace`
(forwarded to the Root Daemon) by the same assignment, so the two daemons can never disagree.
See [02-dual-daemon.md](02-dual-daemon.md) §2.3 for the lock-step invariant.

## Sync Mode Propagation

`SyncRequest` has **no** `ManagerNamespace` field, and the `kubevpn sync` CLI has no
`--manager-namespace` flag. A naive sync clone would therefore run its injected
`kubevpn proxy` sidecar (`genVPNContainer`) without `--manager-namespace`, forcing that
nested proxy to re-detect — which falls back to the **workload** namespace when the manager
lives elsewhere, producing a wrong `TrafficManagerService` (`kubevpn-traffic-manager.<workloadNs>`)
in the envoy sidecar it injects.

To avoid this, the `Sync` daemon action reads the **authoritative** `ManagerNamespace` from
the already-established connection (`svr.findConnection(connectionID).ManagerNamespace`, set by
the inner Connect's detection) into `SyncOptions.ManagerNamespace`. `prepareSyncPodSpec` then
passes it to `genVPNContainer`, which appends `--manager-namespace <ns>` (only when non-empty)
so the nested proxy pins the correct namespace instead of re-detecting. See
[26-sync-mode.md](26-sync-mode.md) §4.4.

## Common Pitfalls

1. **Don't use `c.ManagerNamespace` for workload operations.** It's the manager namespace. Use the `namespace` parameter passed to `CreateRemoteInboundPod()` or `c.WorkloadNamespace`.

2. **Don't use `m.ns` (Mapper) for ConfigMap operations.** The ConfigMap lives in the manager namespace. The Mapper's `cmInformer` already points to the correct namespace.

3. **`req.Namespace` ≠ `connect.ManagerNamespace`.** The RPC field `req.Namespace` is the workload namespace, but `connect.ManagerNamespace` is set to `req.ManagerNamespace`.

4. **The shared ConfigMap informer uses `c.ManagerNamespace` (manager).** This is correct because the ConfigMap lives in the manager namespace. All consumers that need the ConfigMap go through this informer.

5. **Sync clones must inherit the resolved manager namespace, not re-detect it.** `SyncOptions.ManagerNamespace` is copied from the established connection and threaded into the nested `kubevpn proxy` via `--manager-namespace`. Letting the nested proxy auto-detect breaks when manager ≠ workload (see "Sync Mode Propagation" above).
