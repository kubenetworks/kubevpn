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
- **VPN container** (`genVPNContainer`): Runs `kubevpn proxy` with `--foreground`, injects kubeconfig from DownwardAPI. Privileged with `NET_ADMIN` capability.
- **Syncthing container** (`genSyncthingContainer`): Runs `kubevpn syncthing --dir {remoteDir}`, shares the EmptyDir volume with the target container.

### 4.3 Kubeconfig Injection

The kubeconfig is stored as a pod annotation and mounted via DownwardAPI. This avoids needing a separate Secret for each sync clone. `ConvertApiServerToNodeIP` converts the API server address to a node-reachable IP for in-cluster access.

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
2. Execute registered rollback functions
3. Wait for original workloads to finish rollout (`RolloutStatus`)

## 7. Daemon RPC Flow

The `Sync` daemon action:
1. Receives `SyncRequest` via gRPC
2. Establishes cluster connection via `Connect` RPC (reuses existing connect flow)
3. Calls `SyncOptions.DoSync()` with the sync-specific parameters
4. On cancellation, calls `Cleanup()` to remove clone workloads

## 8. Related Files

| File | Purpose |
|------|---------|
| `pkg/handler/sync.go` | SyncOptions, DoSync, prepareSyncPodSpec, Cleanup |
| `pkg/handler/sync_containers.go` | genSyncthingContainer, genVPNContainer, SyncDir |
| `pkg/syncthing/syncthing.go` | StartClient, StartServer |
| `pkg/syncthing/api.go` | REST API client for config updates |
| `pkg/daemon/action/sync.go` | Sync daemon RPC handler |
| `cmd/kubevpn/cmds/sync.go` | CLI command definition |
| `docs/23-syncthing-file-sync.md` | Syncthing library integration |
