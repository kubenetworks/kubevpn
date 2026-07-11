# Rule OwnerID Design

## 1. Problem Statement

### 1.1 Current State

KubeVPN supports multiple users proxying the same K8s workload simultaneously. Each user's proxy rules are stored in the `controlplane.Rule` struct, serialized as YAML and saved in the cluster ConfigMap (`kubevpn-traffic-manager`) under the `ENVOY_CONFIG` key.

Data model:

```
ConfigMap
  └── ENVOY_CONFIG (YAML)
       └── []Virtual
            ├── UID: "deployments.apps.web"
            ├── Namespace: "default"
            └── Rules:
                 ├── Rule[0]: {Headers: {version: v1}, LocalTunIPv4: "198.18.0.5", OwnerID: "a1b2c3d4e5f6", ...}
                 └── Rule[1]: {Headers: {version: v2}, LocalTunIPv4: "198.18.0.9", OwnerID: "f6e5d4c3b2a1", ...}
```

### 1.2 Ownership Determination

All modes (standard/Fargate) use a unified match: `rule.OwnerID == connect.OwnerID`:

| Scenario | Match Logic | Code Location |
|----------|-------------|---------------|
| Leave (unpatch) | `ownerID == rule.OwnerID` | `inject/envoy.go:removeEnvoyConfig` |
| Status (CurrentDevice) | `rule.OwnerID == connect.OwnerID` | `daemon/action/status.go` |

The previous dual-branch logic — IP matching for standard mode, Headers matching for Fargate mode — has been unified.

## 2. Design

### 2.1 OwnerID Field

An explicit owner identifier field in the `Rule` struct:

```go
type Rule struct {
    Headers      map[string]string
    LocalTunIPv4 string
    LocalTunIPv6 string
    OwnerID      string `yaml:"ownerID,omitempty" json:"ownerID,omitempty"`
    PortMap      map[int32]string
}
```

### 2.2 OwnerID Value

Uses a **UUID (first 12 characters)** as the OwnerID value, generated per connection in the User Daemon's `redirectConnectToSudoDaemon`:

```go
// daemon/action/connect_elevate.go
OwnerID: uuid.New().String()[:12],  // e.g., "a1b2c3d4e5f6"
```

Transmitted to the Root Daemon via the `ConnectRequest.OwnerID` proto field.

**Why UUID instead of other identifiers?**

| Approach | Problem |
|----------|---------|
| LocalTunIPv4 | IP can be reassigned to another user by DHCP, causing false matches |
| ConnectionID (namespace UID) | Namespace-level identifier; cannot distinguish multiple users in the same namespace |
| UUID | Globally unique, unaffected by IP reassignment |

### 2.3 OwnerID Propagation in the Dual-Daemon Architecture

```
User Daemon:
  OwnerID = uuid.New().String()[:12]         ← generated
  → req.OwnerID = connect.OwnerID            ← written to ConnectRequest
  → CreateRemoteInboundPod(..., ownerID)     ← injected into envoy rule
  → LeaveResource(..., ownerID)              ← used to remove envoy rule

Root Daemon:
  connect.OwnerID = req.OwnerID              ← received from request
  → NetworkManager.rentIP(OwnerID)           ← used for TunConfigService IP allocation
```

### 2.4 Four Write Scenarios

The `addVirtualRule` function handles 4 scenarios, each correctly setting the OwnerID:

```
Case 1: Brand new workload (index < 0)
  → Create Virtual + Rule, set OwnerID = uuid

Case 2: Same user update (IP match, non-Fargate)
  → Merge Headers/PortMap; backfill OwnerID if empty (backward compatibility)

Case 3: Ownership transfer (Headers match)
  → Update LocalTunIPv4/v6 and OwnerID = new user's uuid

Case 4: New user joins (different Headers, different IP)
  → Append new Rule, set OwnerID = uuid
```

### 2.5 Delete Scenario

`removeEnvoyConfig` matches and deletes directly by `ownerID`:

```go
for i := 0; i < len(virtual.Rules); i++ {
    if ownerID == virtual.Rules[i].OwnerID {
        found = true
        virtual.Rules = append(virtual.Rules[:i], virtual.Rules[i+1:]...)
        i--
    }
}
```

No `isFargateMode` branch is needed — OwnerID matching works for all modes.

### 2.6 Backward Compatibility

| Scenario | Behavior |
|----------|----------|
| New version reads old ConfigMap | OwnerID deserializes to `""` (zero value), no impact on existing logic |
| Old version reads new ConfigMap | OwnerID field is ignored (Go YAML parser ignores unknown fields) |
| New version updates old Rule | Case 2 automatically backfills OwnerID when `OwnerID == ""` |
| New version leaves old Rule (OwnerID is empty) | Will not match (`uuid != ""`); requires a proxy operation first to backfill OwnerID |

The `omitempty` tag ensures empty OwnerID values are not written to YAML, reducing ConfigMap size.

## 3. ConfigMap YAML Example

```yaml
- schemaVersion: 1
  uid: deployments.apps.web
  namespace: default
  ports:
  - containerPort: 8080
    protocol: TCP
  rules:
  - headers:
      version: v1
    localtunipv4: "198.18.0.5"
    localtunipv6: "2001:2::5"
    ownerID: "a1b2c3d4e5f6"
    portmap:
      8080: "9090"
```

`ownerID` is the first 12 characters of a UUID, independent of `localtunipv4`. Even if the IP is reassigned to another user, the OwnerID still uniquely identifies the original creator.

## 4. Related Files

| File | Purpose |
|------|---------|
| `pkg/controlplane/cache.go` | `Rule.OwnerID` field definition |
| `pkg/inject/envoy.go` | `addVirtualRule` writes OwnerID; `removeEnvoyConfig` matches and deletes by OwnerID |
| `pkg/inject/mesh.go` | `UnpatchContainer` receives ownerID parameter |
| `pkg/handler/proxy_manager.go` | `Leave/LeaveAll` passes ownerID to `UnpatchContainer` |
| `pkg/daemon/action/status.go` | `CurrentDevice` uses `rule.OwnerID == connect.OwnerID` for matching |

## 5. Risk Assessment

| Risk | Severity | Mitigation |
|------|----------|------------|
| Old client versions do not understand OwnerID | Low | `omitempty` + Go YAML ignores unknown fields |
| Old Rules without OwnerID cannot be left | Low | Case 2/3 automatically backfills OwnerID during proxy operations |
| ConfigMap size increase | Very Low | ~30 bytes added per Rule |
