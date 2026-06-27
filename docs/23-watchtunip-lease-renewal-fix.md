# WatchTunIP 长连接 Lease 续租修复

## 1. 问题

`TunConfigServer.WatchTunIP` 建立 gRPC stream 后，不刷新 `LastRenew` 时间戳。只有 `GetTunIP` 会刷新。

当 client/sidecar 正常连接时，使用 `WatchTunIP` stream 监听 IP 变更，不会定期调用 `GetTunIP`。
超过 `LeaseDuration`（5 分钟）后，`StartLeaseReaper` 后台 goroutine 判定该分配过期，调用 `dhcp.ReleaseIP` 回收 IP。

**后果：**
- IP 被回收到 DHCP 池，可能被分配给新 client
- 原 client 的 TUN 设备仍在使用旧 IP → IP 冲突
- heartbeat 继续发送旧 IP → RouteHub 状态混乱

## 2. 影响范围

| 端 | Watch 代码位置 | 是否受影响 |
|---|---|---|
| Sidecar | `pkg/core/protocol_registry.go:watchTunIPChanges` | ✅ stream 正常时不调 GetTunIP |
| Client (Root Daemon) | `pkg/handler/network.go:doWatchTunIP` | ✅ stream 正常时不调 GetTunIP |

只有降级到 `pollTunIP`（stream 断连时）才会调用 `GetTunIP`，此时才续租。

## 3. 修复方案

**在 server 端 `WatchTunIP` 中隐式续租：** stream 活跃即视为 client 存活，定期刷新 `LastRenew`。

### 3.1 变更：`pkg/controlplane/tun_config.go`

新增 `renewLease` 方法（要求调用者持有 `s.mu`）：

```go
func (s *TunConfigServer) renewLease(ownerID string) {
    if alloc, ok := s.allocs[ownerID]; ok {
        alloc.LastRenew = time.Now()
    }
}
```

修改 `WatchTunIP`：

```go
func (s *TunConfigServer) WatchTunIP(req *rpc.TunIPRequest, stream ...) error {
    ch := make(chan *rpc.TunIPResponse, 4)

    s.mu.Lock()
    s.watchers[req.OwnerID] = append(s.watchers[req.OwnerID], ch)
    s.renewLease(req.OwnerID)  // 建立连接时立即续租
    s.mu.Unlock()

    defer func() { ... }()

    ticker := time.NewTicker(LeaseDuration / 3)  // 每 100s 续租一次
    defer ticker.Stop()

    for {
        select {
        case resp := <-ch:
            stream.Send(resp)
        case <-ticker.C:
            s.mu.Lock()
            s.renewLease(req.OwnerID)  // 定期续租
            s.mu.Unlock()
        case <-stream.Context().Done():
            return nil
        }
    }
}
```

### 3.2 续租时机

| 事件 | 刷新 LastRenew |
|------|---------------|
| `GetTunIP` 调用 | ✅ (已有) |
| `WatchTunIP` stream 建立 | ✅ (新增) |
| `WatchTunIP` ticker (每 100s) | ✅ (新增) |
| Stream 断开 | ❌ 停止续租 → 5min 后 LeaseReaper 回收 |

### 3.3 为什么用 `LeaseDuration / 3`

- `LeaseDuration = 5 min`，ticker 间隔 ≈ 100s
- 保证在 5 分钟 TTL 内至少续租一次（留 2/3 余量）
- 与标准 DHCP 续租策略一致（T1 = 50% lease time, T2 = 87.5%）

## 4. 不需要改动 client 端

修复在 server 端完成。client/sidecar 端的 `watchTunIPChanges` 和 `doWatchTunIP` 无需改动 — stream 保持活跃时，server 自动续租。

## 5. 生命周期（修复后）

```
T=0:    GetTunIP → 分配 IP, LastRenew = now
T=0:    WatchTunIP stream 建立 → renewLease (LastRenew = now)
T=100s: ticker → renewLease (LastRenew = now)
T=200s: ticker → renewLease (LastRenew = now)
T=300s: ticker → renewLease (LastRenew = now)
...
T=N:    Stream 断开 → ticker 停止
T=N+5m: LeaseReaper → 过期 → ReleaseIP → IP 回收
```

## 6. 文件变更

| 文件 | 变更 |
|------|------|
| `pkg/controlplane/tun_config.go` | 新增 `renewLease` 方法；`WatchTunIP` 中加入初始续租和定期 ticker |
