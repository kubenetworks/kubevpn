# Local State Persistence Design

## Problem

KubeVPN daemon 当前通过 `~/.kubevpn/daemon/db` 持久化连接状态，但只序列化了 `ConnectRequest` 和 `OwnerID`。TUN IP 不再持久化（由 Root Daemon 的 NetworkManager 在启动时通过 OwnerID 重新从 TunConfigService 获取）。以下关键状态在 daemon 重启后丢失：

| 丢失的状态 | 影响 |
|-----------|------|
| `proxyWorkloads` (代理的工作负载列表) | 注入的 sidecar 容器无法被自动清理或重新代理 |
| `SyncOptions` (同步元数据) | 同步克隆的工作负载变成孤儿，无法清理 |
| `currentConnectionID` (当前选中的连接) | 用户需重新 `kubevpn connect use` |
| 集群 CIDR 缓存 | 重连需要重新请求 K8s API |

## Design

### File Layout

```
~/.kubevpn/
├── daemon/
│   ├── state.yaml          ← NEW: 结构化状态文件 (替代旧 db 文件)
│   ├── user_daemon.sock
│   ├── user_daemon.pid
│   ├── root_daemon.sock
│   ├── root_daemon.pid
│   └── syncthing/
├── log/
└── temp/
```

### State Schema

```go
// pkg/config/state.go

// DaemonState is the top-level structure persisted to ~/.kubevpn/daemon/state.yaml.
// It captures all state needed to restore daemon functionality after a restart.
type DaemonState struct {
    Version             string            `yaml:"version"`             // schema version for forward compat
    CurrentConnectionID string            `yaml:"currentConnectionID"` // currently selected connection
    Connections         []ConnectionState `yaml:"connections"`         // all active connections
    LastUpdated         time.Time         `yaml:"lastUpdated"`         // for staleness detection
}

// ConnectionState captures one connection's full state.
type ConnectionState struct {
    // Connection identity
    ConnectionID string `yaml:"connectionID"`
    ClusterName  string `yaml:"clusterName"`  // human-readable cluster name
    Namespace    string `yaml:"namespace"`     // manager namespace

    // TUN IP 不再持久化 — Root Daemon 通过 OwnerID 从 TunConfigService 重新获取

    // The original ConnectRequest — contains kubeconfig, SSH config, extra routes, etc.
    // This is the only thing currently persisted; kept for backward compatibility.
    Request *rpc.ConnectRequest `yaml:"request"`

    // Proxy state — NEW: which workloads are being proxied
    Proxies []ProxyState `yaml:"proxies,omitempty"`

    // Sync state — NEW: which workloads are being synced
    Syncs []SyncState `yaml:"syncs,omitempty"`

    // Cached cluster info — NEW: avoids API calls on reconnect
    CachedCIDRs []string `yaml:"cachedCIDRs,omitempty"` // e.g. ["10.96.0.0/12", "10.244.0.0/16"]
}

// ProxyState captures one proxied workload's metadata.
type ProxyState struct {
    Namespace string            `yaml:"namespace"`
    Workload  string            `yaml:"workload"`  // e.g. "deployments.apps/reviews"
    Headers   map[string]string `yaml:"headers,omitempty"`
    PortMaps  []string          `yaml:"portMaps,omitempty"`
}

// SyncState captures one synced workload's metadata.
type SyncState struct {
    Namespace       string            `yaml:"namespace"`
    Workload        string            `yaml:"workload"`        // original workload name
    TargetWorkload  string            `yaml:"targetWorkload"`  // the -sync- clone name
    LocalDir        string            `yaml:"localDir"`
    RemoteDir       string            `yaml:"remoteDir"`
    Headers         map[string]string `yaml:"headers,omitempty"`
    TargetContainer string            `yaml:"targetContainer,omitempty"`
}
```

### State File Example

```yaml
version: "1"
currentConnectionID: "a1b2c3d4e5f6"
lastUpdated: "2026-06-05T12:00:00Z"
connections:
  - connectionID: "a1b2c3d4e5f6"
    clusterName: "production-ack"
    namespace: "kubevpn"
    localTunIPv4: "198.18.0.100/32"
    localTunIPv6: "2001:2::100/128"
    request:
      kubeconfigBytes: "..."
      namespace: "default"
      # ... (existing ConnectRequest fields)
    proxies:
      - namespace: "default"
        workload: "deployments.apps/reviews"
        headers:
          env: "test"
        portMaps:
          - "9080:9080"
      - namespace: "default"
        workload: "services/productpage"
        headers:
          user: "dev1"
    syncs:
      - namespace: "default"
        workload: "deployments.apps/ratings"
        targetWorkload: "ratings-sync-abc123"
        localDir: "/home/user/ratings"
        remoteDir: "/app"
        headers:
          env: "dev"
    cachedCIDRs:
      - "10.96.0.0/12"
      - "10.244.0.0/16"
```

### API

```go
// pkg/config/state.go

// StatePath returns the path to the daemon state file.
func StatePath() string {
    return filepath.Join(daemonPath, "state.yaml")
}

// LoadState reads and deserializes the daemon state from disk.
// Returns empty state (not error) if file does not exist.
func LoadState() (*DaemonState, error)

// SaveState atomically writes the daemon state to disk.
// Uses write-to-temp + rename for crash safety.
func SaveState(state *DaemonState) error
```

```go
// pkg/daemon/action/persistence.go

// SaveState serializes current server state to the state file.
// Called after Connect, Disconnect, Proxy, Sync, Leave, Unsync.
func (svr *Server) SaveState() error {
    state := &config.DaemonState{
        Version:             "1",
        CurrentConnectionID: svr.currentConnectionID,
        LastUpdated:         time.Now(),
    }
    for _, conn := range svr.connections {
        cs := config.ConnectionState{
            ConnectionID: conn.GetConnectionID(),
            Namespace:    conn.Namespace,
            Request:      conn.Request,
        }
        // TUN IP 不再持久化 — 由 Root Daemon 的 NetworkManager 通过 OwnerID 管理
        // proxy workloads
        for _, proxy := range conn.ProxyResources() {
            cs.Proxies = append(cs.Proxies, config.ProxyState{
                Namespace: proxy.namespace,
                Workload:  proxy.workload,
                Headers:   proxy.headers,
                PortMaps:  proxy.portMap,
            })
        }
        // sync options
        if conn.Sync != nil {
            for _, w := range conn.Sync.Workloads {
                cs.Syncs = append(cs.Syncs, config.SyncState{
                    Namespace:      conn.Sync.Namespace,
                    Workload:       w,
                    TargetWorkload: conn.Sync.TargetWorkloadNames[w],
                    LocalDir:       conn.Sync.LocalDir,
                    RemoteDir:      conn.Sync.RemoteDir,
                    Headers:        conn.Sync.Headers,
                })
            }
        }
        state.Connections = append(state.Connections, cs)
    }
    return config.SaveState(state)
}

// RestoreState reads the state file and re-establishes connections.
// Replaces the current LoadFromConfig.
func (svr *Server) RestoreState() error
```

### Save Triggers

状态在以下操作后自动保存：

| Operation | Trigger |
|-----------|---------|
| `Connect` (成功) | `svr.SaveState()` |
| `Disconnect` (成功) | `svr.SaveState()` |
| `Proxy` (成功) | `svr.SaveState()` |
| `Leave` (成功) | `svr.SaveState()` |
| `Sync` (成功) | `svr.SaveState()` |
| `Unsync` (成功) | `svr.SaveState()` |
| `Quit` | Delete state file |

### Crash Safety

`SaveState` 使用 atomic write 模式：

```go
func SaveState(state *DaemonState) error {
    data, err := yaml.Marshal(state)
    if err != nil {
        return err
    }
    tmp := StatePath() + ".tmp"
    if err := os.WriteFile(tmp, data, 0600); err != nil {
        return err
    }
    return os.Rename(tmp, StatePath())
}
```

`os.Rename` 在同一文件系统上是原子的。即使 daemon 在写入过程中崩溃，要么旧状态完整保留，要么新状态完整写入，不会出现半写状态。

### Backward Compatibility

1. **旧 → 新迁移**: `RestoreState()` 首先尝试读取 `state.yaml`，如果不存在则尝试旧的 `db` 文件（现有的 `LoadFromConfig` 逻辑），迁移成功后删除旧 `db` 文件。

2. **Schema versioning**: `version: "1"` 字段用于未来 schema 变更。读取时检查 version，不匹配则跳过恢复并记录警告。

3. **字段扩展**: 所有新增字段使用 `omitempty`，旧版 state 文件缺少的字段自然为零值。

### Security

- State file permissions: `0600` (owner read/write only)
- `kubeconfigBytes` in `Request` contains sensitive cluster credentials — same security model as the existing `db` file
- State file path under `~/.kubevpn/daemon/` which is user-owned

### Restore Flow

```
Daemon starts
  │
  ├─ Read state.yaml (or migrate from db)
  │
  ├─ For each connection:
  │   ├─ Re-issue Connect RPC with saved Request + IPv4/IPv6
  │   ├─ Restore currentConnectionID
  │   │
  │   ├─ For each proxy:
  │   │   └─ Log warning: "workload X was being proxied, sidecar may still be injected"
  │   │      (不自动重新 proxy，因为本地端口可能已被占用)
  │   │
  │   └─ For each sync:
  │       └─ Log warning: "workload X was being synced as Y"
  │          (不自动重新 sync，用户需手动决定)
  │
  └─ If state is stale (lastUpdated > 24h), warn and skip restore
```

### What NOT to Persist

| State | Reason |
|-------|--------|
| K8s client objects | Runtime objects, reconstructed from kubeconfig |
| TUN device name | OS-level resource, must be recreated |
| rollbackFuncList | Closures, not serializable |
| DNS config | Platform-specific, must be reconstructed |
| Health status | Ephemeral, meaningless after restart |
| SSH tunnel/port-forward | Must be re-established |
| Mapper (SSH tunnels) | Runtime goroutines, driven by ConfigMap state |
| Context/cancel | Runtime lifecycle |
