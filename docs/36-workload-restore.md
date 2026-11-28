# Workload Restore (Leave & Reset)

## 1. Overview

Node-boundary attachment does not modify workload PodTemplates, Services, or
`targetPort`. Leave and reset therefore remove control-plane state only; neither
operation rolls or patches a workload.

- **`kubevpn leave`** — ownership-scoped removal of the current connection's
  rules, matched by `OwnerID`. Other users' rules are preserved.
- **`kubevpn reset`** — unconditional removal of all rules for the named
  workloads, regardless of owner. Legacy sidecar Virtuals are rejected rather
  than migrated or silently cleaned.

The manager's `AttachmentPublisher` removes derived Virtuals and
`NODE_ATTACHMENTS` after the last base rule disappears. Allocation tombstones
remain to prevent immediate listener-port reuse.

## 2. Leave (ownership-scoped)

```
RPC Leave (action/leave.go)
  └── findConnection(currentConnectionID)
      └── conn.LeaveResource(ctx, resources, conn.OwnerID)
            └── manager LeaveInject
                  for each workload:
                    ├── resolve the live workload when available
                    ├── otherwise resolve the stored Virtual UID/alias
                    ├── VirtualStore.RemoveRule(ns, nodeID, ownerID)
                    ├── !found  ⇒ idempotent no-op
                    ├── !empty  ⇒ other owners remain
                    └── empty   ⇒ publisher removes derived attachments
```

A deleted workload or Service does not block cleanup: leave can resolve its
identity from the stored Virtual instead of issuing a required GET. Short aliases
such as `deploy/foo` still resolve through the live workload when available and
fall back to the stored identity otherwise.

There is no client-side ProxyManager, SSH Mapper, container removal, rollout
wait, or Service target-port restoration.

## 3. Reset (unconditional)

```
RPC Reset (action/reset.go)
  ├── resolveKubeconfigBytes + InitClient + DetectManagerNamespace
  └── connect.Reset(ns, workloads)
        └── resetConfigMap(managerNS, ns, workloads)
              ├── read ConfigMap ENVOY_CONFIG
              ├── unmarshal []*controlplane.Virtual
              ├── reject selected legacy sidecar Virtuals
              ├── drop every matching base Virtual (ALL owners)
              └── write back
```

Reset does not call `GetTopOwnerObject`, `RemoveContainers`, `patchWorkload`,
`RolloutStatus`, or `RestoreServiceTargetPort`. Uninstall continues to delete
manager and node-agent resources only; it does not migrate old injected
workloads.
