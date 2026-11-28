# Sync Mode Design

## 1. Overview

Sync mode (`kubevpn sync`) creates a clone of a Kubernetes workload with syncthing-based bidirectional file synchronization. It connects to the cluster network, synchronizes a local directory with the cloned pod, and routes matching traffic from the original workload to the clone through the node-boundary agent.

The original workload is never patched or restarted. The clone is the only workload whose PodSpec is modified.

## 2. Architecture

```shell cli-checked
kubevpn sync deployment/myapp --sync ./src:/app
```

```
  ┌─ User Daemon / Control Session ─────────────────────────┐
  │  1. Connect to the cluster                              │
  │  2. DoSync:                                             │
  │     a. Clone workload → myapp-sync-{uuid}               │
  │     b. Modify only the target application container     │
  │     c. Add the Syncthing helper and shared EmptyDir     │
  │     d. Create clone and wait for its pod to be Ready    │
  │     e. Start local Syncthing client and IP tracker      │
  │  3. CreateRemoteSyncRoutes through the existing manager │
  │     connection and OwnerID                              │
  └─────────────────────────────────────────────────────────┘
                             │
                             ▼
  ┌─ Traffic manager / AttachmentPublisher ─────────────────┐
  │  ENVOY_CONFIG: original workload + declarative          │
  │  Rule.SyncTarget(clone UID, clone selector)             │
  │                                                         │
  │  AGENT_XDS_CONFIG: resolve the newest Ready clone pod   │
  │  and route matched traffic to clonePodIP                │
  └─────────────────────────────────────────────────────────┘
```

There is no VPN/Envoy container, nested `kubevpn proxy`, nested DHCP lease, or kubeconfig inside the sync clone. Syncthing remains as a file-synchronization helper; it is not a traffic-attachment sidecar.

## 3. SyncOptions

```go
type SyncOptions struct {
    K8sClient
    WorkloadNamespace     string
    Headers               map[string]string
    Workloads             []string
    TargetContainer       string
    TargetImage           string
    TargetWorkloadNames   map[string]string // original → clone name
    LocalDir              string
    RemoteDir             string
}
```

`SyncOptions` adapts the daemon session to `internal/syncworkload.Runtime`. The runtime owns cloned workloads, local Syncthing clients, and Pod IP trackers. The outer `ProxyController` owns manager route creation/removal. `TargetWorkloadNames` is copied back after `DoSync`, including on failure, and `GetSyncthingGUIAddr` reads runtime status.

The manager namespace is already resolved by the outer Connect. Sync does not start another connection and therefore does not propagate a separate manager namespace into the clone.

## 4. Clone and Route Creation

### 4.1 Workload cloning

1. Resolve the top-level owner (Deployment/StatefulSet) of the source workload.
2. Deep-copy the object, force one replica, and clear resource version, UID, status, and managed fields.
3. Name it `{original}-sync-{uuid[:5]}`.
4. Give it unique labels: `manage-by`, `owner-ref`, and `origin-workload`; use those labels for the clone selector.
5. Reject hostNetwork or legacy sidecar-injected sources before creating the clone.

### 4.2 Pod template modifications

- Add one `EmptyDir` shared by the target application and Syncthing helper.
- Optionally replace the target image.
- When `LocalDir` is set, mount the shared directory, replace the application command with `tail -f /dev/null`, and remove health probes.
- Add only `genSyncthingContainer`. Do not add VPN/Envoy containers, kubeconfig annotations, DownwardAPI kubeconfig volumes, or lifecycle networking hooks.

### 4.3 Declarative sync routes

After all clones and local clients are ready, the sync action submits original→clone `SyncRoute` entries over the existing manager connection.

For each original workload, the base Virtual records:

- the original workload UID and pod selector;
- the outer connection OwnerID and requested headers;
- identity port mappings;
- `Rule.SyncTarget{UID, Selector}` for the clone.

The base rule does not store a resolved clone PodIP. `ENVOY_CONFIG` contains only these desired base rules; `AGENT_XDS_CONFIG` contains the derived per-node projection. `AttachmentPublisher` reads desired state but never writes it, and is the sole writer of the derived projection.

On every reconcile, `AttachmentPublisher`:

1. Builds normal source pod/Service attachments for the original workload.
2. Lists Ready, Running, non-deleting clone pods using `SyncTarget.Selector`.
3. Selects the newest Ready pod deterministically.
4. Deep-copies rules into each derived per-node Virtual and writes the clone PodIP only into that derived rule.
5. Never creates a NodeAttachment, host DNAT rule, Envoy process, or DHCP lease for the clone itself.

`xds/internal/envoy/Rules.Sync` skips sync-target rules so a user DHCP renewal cannot overwrite the clone endpoint. A clone List failure preserves the last good ConfigMap; an absent Ready clone never leaves a stale resolved address. Initial attach waits for both source direct listeners and a resolved derived sync rule.

## 5. Local Syncthing Client

After the clone pod is ready, the runtime:

1. Reads its PodIP and allocates a random local TCP port.
2. Starts `syncthing.StartClient` and retains its `ClientHandle`.
3. Logs the local GUI URL.
4. Polls the clone every two seconds and updates the remote device address after pod recreation.

`ClientHandle.Stop()` cancels and waits for the application and early service supervisor. Runtime cleanup joins all clients and IP trackers without cancelling the parent session before route and clone cleanup have finished.

## 6. Cleanup

Cleanup is deliberately ordered:

1. Map requested clone names back to their source workloads (or select all sources).
2. Remove the source sync rules through the outer `ProxyController` and OwnerID.
3. Only after route removal succeeds, delete the selected clone workloads.
4. Stop and join local Syncthing applications, early services, and Pod IP trackers.
5. Execute session/SSH rollback functions.

If route removal fails, the clone is preserved for retry and no stale route is deliberately pointed at a deleted backend. The original workload has no rollout to wait for because sync never modifies it. Disconnect cleanup and manager lease reaping use the same OwnerID ownership model.

## 7. Daemon RPC Flow

1. Receive `SyncRequest` and establish the normal outer Connect.
2. Initialize `SyncOptions` and create/wait for clones and local Syncthing clients.
3. Build `handler.SyncRoute` values from `Workloads` and `TargetWorkloadNames`.
4. Call `ProxyController.CreateRemoteSyncRoutes`; the manager resolves workload selectors and writes declarative sync rules.
5. Refresh the ConfigMap cache and attach `SyncOptions` to the control session for status and cleanup.

The public `SyncRequest` and CLI flags are unchanged. `InjectRequest.SyncRoutes` is an internal manager API and cannot be mixed with ordinary `Workloads` in one request.

## 8. Related Files

| File | Purpose |
|------|---------|
| `pkg/handler/sync.go` | SyncOptions, route cleanup ordering, runtime adapter |
| `pkg/handler/internal/syncworkload/runtime.go` | Clone orchestration and local-client lifecycle |
| `pkg/handler/internal/syncworkload/podspec.go` | Application/Syncthing-only clone mutation |
| `pkg/handler/internal/syncworkload/containers.go` | Syncthing helper, local sync and Pod IP tracking |
| `pkg/handler/inject_client.go` | Outer manager SyncRoute client |
| `pkg/controlplane/virtual.go` | Declarative `SyncTarget` model |
| `pkg/xds/attachment_publisher.go` | Clone endpoint resolution in derived Virtuals |
| `pkg/xds/inject_service.go` | Internal SyncRoute admission |
| `pkg/daemon/action/sync.go` | Outer Connect → clone → route orchestration |
| `docs/23-syncthing-file-sync.md` | Syncthing library integration |
| `docs/55-node-hosted-agent.md` | Node-boundary attachment architecture |
