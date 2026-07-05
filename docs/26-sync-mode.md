# Sync Mode Design

## 1. Overview

Sync mode (`kubevpn sync`) creates a clone of a Kubernetes workload with syncthing-based bidirectional file synchronization. It connects to the cluster network and synchronizes a local directory with a remote directory in the cloned pod, enabling live code reloading during development.

## 2. Architecture

```
kubevpn sync deployment/myapp --local-dir ./src --remote-dir /app

  ┌─ User Daemon ─────────────────────────────────────────┐
  │                                                       │
  │  1. Connect to cluster (via Connect RPC)              │
  │  2. DoSync:                                           │
  │     a. Clone workload → myapp-sync-{uuid}             │
  │     b. Add VPN sidecar (kubevpn proxy --foreground)   │
  │     c. Add Syncthing sidecar (kubevpn syncthing)      │
  │     d. Modify target container:                       │
  │        - Mount EmptyDir volume at /app                │
  │        - Replace command with tail -f /dev/null       │
  │        - Remove health probes                         │
  │     e. Create clone in cluster                        │
  │     f. Wait for pod ready                             │
  │     g. Start local syncthing client                   │
  │  3. Watch for pod IP changes (reconnect syncthing)    │
  │                                                       │
  └───────────────────────────────────────────────────────┘
```

## 3. SyncOptions

```go
type SyncOptions struct {
    K8sClient                       // embedded K8s client
    WorkloadNamespace     string
    ManagerNamespace      string    // resolved traffic manager ns; threaded into nested proxy
    Headers               map[string]string
    Workloads             []string
    TargetContainer       string    // which container to sync into
    TargetImage           string    // optional image override
    TargetWorkloadNames   map[string]string  // original → clone name mapping
    LocalDir              string    // local directory to sync
    RemoteDir             string    // remote directory in container
}
```

## 4. Clone Workload Creation (`DoSync`)

### 4.1 Workload Cloning

1. Get the top-level owner (Deployment/StatefulSet) of the target workload
2. Deep-copy the unstructured object
3. Modify metadata:
   - Name: `{original}-sync-{uuid[:5]}`
   - Labels: `manage-by: kubevpn-traffic-manager`, `owner-ref`, `origin-workload`
   - Replicas: forced to 1
   - Clear resource version, UID, managed fields
4. Replace selector matchLabels with the new labels

### 4.2 Pod Template Modifications (`prepareSyncPodSpec`)

The clone pod gets three types of modifications:

**Volumes added:**
- `kubeconfig` — DownwardAPI volume mounting kubeconfig from pod annotation
- `syncthing` — EmptyDir shared between target container and syncthing sidecar

**Target container modified (when `LocalDir` is set):**
- Command replaced with `tail -f /dev/null` (keeps pod alive without running app)
- `remoteDir` mounted from the shared EmptyDir volume
- Health probes removed (liveness, readiness, startup)

**Sidecar containers added:**
- **VPN container** (`genVPNContainer`): Runs `kubevpn proxy` with `--foreground`, injects kubeconfig from DownwardAPI. Privileged with `NET_ADMIN` capability. When `SyncOptions.ManagerNamespace` is set, the command also carries `--manager-namespace <ns>` (see §4.4).
- **Syncthing container** (`genSyncthingContainer`): Runs `kubevpn syncthing --dir {remoteDir}`, shares the EmptyDir volume with the target container.

### 4.3 Kubeconfig Injection

The kubeconfig is stored as a pod annotation and mounted via DownwardAPI. This avoids needing a separate Secret for each sync clone. `ConvertApiServerToNodeIP` converts the API server address to a node-reachable IP for in-cluster access.

### 4.4 Manager Namespace Propagation

The cloned workload runs a **nested** `kubevpn proxy <workload> --namespace <workloadNs>`
inside the VPN sidecar. That nested proxy is the component that injects the envoy sidecar
into the proxied workload and sets its `TrafficManagerService` env
(`kubevpn-traffic-manager.<managerNs>`).

`SyncRequest` carries no `ManagerNamespace`, and the `sync` CLI exposes no
`--manager-namespace` flag. If the nested proxy were left to auto-detect, it would fall back
to the **workload** namespace whenever the traffic manager lives elsewhere (manager ≠
workload, e.g. a Helm install in a dedicated namespace), pointing envoy at a non-existent
`kubevpn-traffic-manager.<workloadNs>` and breaking the control-plane connection.

To prevent this, the resolved manager namespace is threaded through explicitly:

1. The `Sync` daemon action runs the inner `Connect` first, which resolves the manager
   namespace (`detectAndSetManagerNamespace`) and stores it on the connection.
2. After `connectionID` is known, the action copies the authoritative value into the sync
   options: `options.ManagerNamespace = svr.findConnection(connectionID).ManagerNamespace`.
3. `prepareSyncPodSpec` passes `d.ManagerNamespace` to `genVPNContainer`, which appends
   `--manager-namespace <ns>` **only when non-empty** (empty preserves the legacy
   auto-detect behavior).

This keeps the sync clone consistent with the rest of the namespace model
([07-namespace-model.md](07-namespace-model.md)). Tests:
`TestSync_PinsManagerNamespace_WhenManagerDiffersFromWorkload`,
`TestSync_OmitsManagerNamespace_WhenUnset`.

## 5. Local Syncthing Client (`SyncDir`)

After the clone pod is ready:

1. Discover pod IP from running pod list
2. Allocate a random local TCP port
3. Start syncthing client: `syncthing.StartClient(ctx, localDir, localAddr, remoteAddr)`
4. Log the GUI URL for status monitoring
5. Start background IP tracker goroutine

### Pod IP Tracking

A background goroutine polls running pods every 2 seconds. When the pod name changes (indicating recreation), it:
1. Gets the new pod IP
2. Calls `client.GetConfig()` / `client.PutConfig()` to update the syncthing remote device address
3. Syncthing reconnects automatically

## 6. Cleanup (`Cleanup`)

1. Delete all sync clone workloads via dynamic client
2. Wait for original workloads to finish rollout (`RolloutStatus`)
3. Execute registered rollback functions

**Ordering matters.** The rollback functions tear down the session — in the daemon
the only registered one calls `session.RunCleanups()` → `session.Cancel()`, which
cancels the context the SSH port-forward is bound to and closes the local tunnel
endpoint that `d.factory` talks through (see [12-session-lifecycle.md](12-session-lifecycle.md)
and [15-ssh-architecture.md](15-ssh-architecture.md)). The `RolloutStatus` wait in
step 2 still needs that connection, so it must run **before** the rollback functions —
otherwise it dials a closed tunnel and fails with `connection refused`.

## 7. Daemon RPC Flow

The `Sync` daemon action:
1. Receives `SyncRequest` via gRPC
2. Establishes cluster connection via `Connect` RPC (reuses existing connect flow)
3. Copies the resolved `ManagerNamespace` from the established connection into `SyncOptions`
   (`svr.findConnection(connectionID).ManagerNamespace`) before sync — see §4.4
4. Calls `SyncOptions.DoSync()` with the sync-specific parameters
5. On cancellation, calls `Cleanup()` to remove clone workloads

## 8. Related Files

| File | Purpose |
|------|---------|
| `pkg/handler/sync.go` | SyncOptions, DoSync, Cleanup |
| `pkg/handler/sync_podspec.go` | `prepareSyncPodSpec` + `syncPodSpec` param object (pod template mutation) |
| `pkg/handler/sync_containers.go` | genSyncthingContainer, genVPNContainer, SyncDir |
| `pkg/syncthing/syncthing.go` | StartClient, StartServer |
| `pkg/syncthing/api.go` | REST API client for config updates |
| `pkg/daemon/action/sync.go` | Sync daemon RPC handler |
| `cmd/kubevpn/cmds/sync.go` | CLI command definition |
| `docs/23-syncthing-file-sync.md` | Syncthing library integration |
