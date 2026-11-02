# Syncthing File Sync Design

## 1. Overview

The syncthing package (`pkg/syncthing`) provides bidirectional file synchronization between local development directories and remote Kubernetes pods. It embeds the Syncthing library (not a standalone binary) and runs it as client/server peers.

## 2. Architecture

```
Local Machine                                Remote Pod (sync clone)
┌────────────────────────────┐               ┌─────────────────────────────┐
│  syncthing.StartClient()   │               │  syncthing container        │
│  ├── LocalDeviceID         │               │  ├── RemoteDeviceID         │
│  ├── FolderType: SendRecv  │  TCP :22000   │  ├── FolderType: SendRecv   │
│  ├── FSWatcher: 10ms delay │◄─────────────►│  ├── syncthing.StartServer()│
│  ├── Rescan: 3s            │               │  ├── remoteDir mounted via  │
│  └── GUI: localhost:random  │               │  │   EmptyDir volume        │
│       ↕                    │               │  └── GUI: 0.0.0.0:8384      │
│  local directory           │               │                             │
│  (user's source code)      │               │  target container           │
│                            │               │  └── remoteDir (same volume)│
└────────────────────────────┘               └─────────────────────────────┘
```

## 3. Client/Server Model

### 3.1 Server (`StartServer`)

Runs inside a sidecar container (`syncthing`) in the sync clone pod:

- **DeviceID**: `RemoteDeviceID` (hardcoded in `pkg/config`)
- **Folder**: Maps `remoteDir` as `SendReceive` (bidirectional sync)
- **Listen**: TCP on port 22000 (Syncthing default)
- **GUI**: `0.0.0.0:8384` (for remote health checks)
- **Storage**: In-memory database (`backend.OpenMemory()`)
- **TLS**: Uses `RemoteCert` from `pkg/config`
- **Global Announce**: Disabled (peer addresses are direct)

### 3.2 Client (`StartClient`)

Runs on the local machine, started by the private `Runtime.syncDir()` in `pkg/handler/internal/syncworkload`:

- **DeviceID**: `LocalDeviceID` (hardcoded in `pkg/config`)
- **Folder**: Maps `localDir` as `SendReceive` with filesystem watcher (10ms delay, 3s rescan)
- **Remote address**: Pod IP + port 22000 (discovered via K8s API)
- **GUI**: `127.0.0.1:<random port>` (for local status monitoring)
- **API Key**: `SyncthingAPIKey` from config
- **TLS**: Uses `LocalCert` from `pkg/config`

`StartClient` returns `(*ClientHandle, error)` after startup. The runtime owns the handle: `Stop()` cancels and waits, `Done()` closes only after the application (`app.Wait`) and its early service supervisor have exited, and `Err()` exposes the completed application's error. Startup failure also cleans up and waits for started resources before returning. Runtime cleanup joins the local clients and pod IP trackers before handler session/SSH rollback. `StartServer` retains its existing `detach` parameter and CLI behavior.

## 4. REST API Client (`api.go`)

`Client` wraps the Syncthing REST API for runtime configuration updates:

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GetConfig` | `GET /rest/config` | Read current syncthing configuration |
| `PutConfig` | `PUT /rest/config` | Update configuration (e.g., change remote address) |

Used by `Runtime.syncDir()` to update the remote peer address when a pod is recreated (new IP).

## 5. Device Identity

Syncthing requires stable device IDs for peer authentication. KubeVPN uses hardcoded certificate pairs:

| Constant | Location | Used By |
|----------|----------|---------|
| `LocalDeviceID` | `pkg/config` | Client instance |
| `RemoteDeviceID` | `pkg/config` | Server instance |
| `LocalCert` | `pkg/config` (embedded) | Client TLS |
| `RemoteCert` | `pkg/config` (embedded) | Server TLS |

The `cert.pem` and `key.pem` files in the syncthing package are the embedded certificates for the remote (server) side.

## 6. Pod IP Tracking

When the sync pod is recreated (e.g., node eviction), the pod IP changes. `Runtime.syncDir()` runs a background goroutine that:

1. Polls running pods matching the sync labels every 2 seconds
2. Detects pod name changes (indicating a new pod)
3. Calls `client.GetConfig()` / `client.PutConfig()` to update the remote device address
4. Syncthing reconnects automatically to the new address

## 7. Related Files

| File | Purpose |
|------|---------|
| `pkg/syncthing/syncthing.go` | StartClient, ClientHandle, StartServer, app lifecycle |
| `pkg/syncthing/api.go` | REST API client for config updates |
| `pkg/handler/internal/syncworkload/containers.go` | genSyncthingContainer, private syncDir, pod IP tracker |
| `pkg/handler/internal/syncworkload/runtime.go` | Clone and local-client ownership, Stop/join, workload cleanup |
| `pkg/handler/sync.go` | SyncOptions configuration, runtime adapter, session rollback and progress output |
| `pkg/config/config.go` | Device IDs, certs, syncthing paths |
