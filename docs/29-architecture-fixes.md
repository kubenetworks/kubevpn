# 架构问题修复设计文档

## 概述

本文档详述对 KubeVPN 架构审查中发现的问题的修复方案。排除了已确认不修的项目（SSH 认证、控制面认证、SPOF、ConfigMap 替换、Socket 权限、mTLS、TLS 私钥暴露、IP 限流、daemon 间授权、线协议长度、镜像 TLS、xDS 流数限制），剩余需修复的问题分为 4 类。

---

## 一、严重：并发安全

### 1.1 GetTunIP mutex 释放窗口导致重复 IP 分配

**现状**：`pkg/controlplane/tun_config.go:193` 和 `:228` 在调用 `dhcp.RentIPExcluding` 前手动 `s.mu.Unlock()`，完成后再 `s.mu.Lock()`。两个并发 `GetTunIP` 可能在窗口内拿到同一 IP。

**修复方案**：保持 mutex 锁住整个 `GetTunIP`，不在中间释放。`RentIPExcluding` 的执行时间是 ConfigMap GET→modify→Update（毫秒级），不需要释放 mutex 来提升并发。

**改动文件**：`pkg/controlplane/tun_config.go`

**改动内容**：

```go
// 修复前（新分配路径，line 227-230）:
s.mu.Unlock()
v4, v6, err := s.dhcp.RentIPExcluding(ctx, excludeIPs)
s.mu.Lock()

// 修复后: 删除 Unlock/Lock，在 defer s.mu.Unlock() 保护下直接调用
v4, v6, err := s.dhcp.RentIPExcluding(ctx, excludeIPs)
```

同样处理重分配路径（line 193-208）：删除 `s.mu.Unlock()` 和 `s.mu.Lock()`，让整个操作在锁保护下执行。

**副作用**：持锁时间从微秒增加到毫秒（ConfigMap Update），但 GetTunIP 的 QPS 极低（每用户仅在连接时调用一次），不会成为瓶颈。

---

### 1.2 HealthStatus 跨 goroutine 读写无同步

**现状**：`pkg/handler/configmap_store.go` 的 `syncFromCache`（line 121-122）和 `HealthCheckOnce`（line 144-145）在后台 goroutine 写 `healthStatus.cm` 和 `healthStatus.lastErr`，`GetHealthStatus`（line 149）在主 goroutine 读取——无锁保护。

**修复方案**：给 `ConfigMapStore` 添加 `sync.RWMutex`，读用 `RLock`，写用 `Lock`。

**改动文件**：`pkg/handler/configmap_store.go`

**改动内容**：

```go
type ConfigMapStore struct {
    // ... existing fields ...
    mu sync.RWMutex // 保护 healthStatus
}

func (s *ConfigMapStore) syncFromCache() {
    items := s.GetInformer().GetStore().List()
    for _, item := range items {
        if cm, ok := item.(*corev1.ConfigMap); ok {
            s.mu.Lock()
            s.healthStatus.lastErr = nil
            s.healthStatus.cm = cm
            s.mu.Unlock()
            return
        }
    }
}

func (s *ConfigMapStore) HealthCheckOnce(ctx context.Context, timeout time.Duration) {
    // ... existing GET logic ...
    s.mu.Lock()
    s.healthStatus.lastErr = err
    if err == nil {
        s.healthStatus.cm = configMap
    }
    s.mu.Unlock()
}

func (s *ConfigMapStore) GetHealthStatus() HealthStatus {
    s.mu.RLock()
    defer s.mu.RUnlock()
    return s.healthStatus
}
```

---

## 二、重要：并发与可靠性

### 2.1 DHCP bitmap 与 TUN_ALLOCS 非原子更新 + saveAllocs 异步吞错误

**现状**：
- `dhcp.RentIPExcluding` 更新 ConfigMap 的 `DHCP` key（bitmap）
- `saveAllocs` 在单独的 goroutine 中更新 `TUN_ALLOCS` key
- 崩溃时两者不一致：bitmap 标记 IP 已占用，但 TUN_ALLOCS 没有对应记录 → IP 永久泄漏
- `saveAllocs` 的错误赋给 `_`，从不记录日志

**修复方案**：

**(a) saveAllocs 改同步 + 记录错误**：

```go
// 修复前:
go s.saveAllocs(context.Background())

// 修复后:
if err := s.saveAllocs(ctx); err != nil {
    plog.G(ctx).Errorf("[TunConfig] Failed to persist allocs: %v", err)
}
```

修改 `saveAllocs` 内部，把 `_ = retry.RetryOnConflict(...)` 改为记录错误：

```go
err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
    // ... existing ConfigMap update logic ...
})
if err != nil {
    plog.G(ctx).Errorf("[TunConfig] saveAllocs ConfigMap update failed: %v", err)
}
return err
```

**(b) 原子写入 DHCP + TUN_ALLOCS**：

修改 `dhcp.Manager.RentIPExcluding` 接受可选的 `extraUpdates map[string]string`，在同一个 `RetryOnConflict` 内同时更新 bitmap 和 TUN_ALLOCS。

但这增加了跨包耦合。更简单的方案是在 `TunConfigServer.GetTunIP` 中，在持锁期间先调用 `RentIPExcluding`（更新 bitmap），然后立即同步调用 `saveAllocs`（更新 TUN_ALLOCS）。虽然仍是两次 ConfigMap Update，但在单一 mutex 保护下，崩溃窗口极小（两次 Update 之间的毫秒级间隔）。

加上 `loadAllocs` 启动时的 reconciliation（已有逻辑：对比 bitmap 与 allocs 的一致性），足以处理极端情况。

**改动文件**：`pkg/controlplane/tun_config.go`

---

### 2.2 reapExpiredLeases 竞态窗口

**现状**：`reapExpiredLeases`（line 423-458）先收集过期 ownerID，释放锁，再逐个重新加锁删除。窗口内客户端可能已重连并分配了新条目，导致新条目被误删。

**修复方案**：重新获取锁后，二次检查 `LastRenew` 是否仍然过期。

```go
for _, ownerID := range expired {
    s.mu.Lock()
    alloc, ok := s.allocs[ownerID]
    if !ok || time.Since(alloc.LastRenew) <= LeaseDuration {
        // 客户端已重连并续约，跳过
        s.mu.Unlock()
        continue
    }
    delete(s.allocs, ownerID)
    s.mu.Unlock()
    // ... release IP ...
}
```

**改动文件**：`pkg/controlplane/tun_config.go`

---

### 2.3 Cleanup sync.Once 不可重试

**现状**：`pkg/handler/cleaner.go:33` 用 `sync.Once` 包裹所有清理逻辑。10 秒超时内未完成的步骤永远不会重试。

**修复方案**：改为 mutex + 完成标志，允许重入式重试。

```go
type ConnectOptions struct {
    // 替换 once sync.Once
    cleanupMu   sync.Mutex
    cleanedUp   bool
}

func (c *ConnectOptions) Cleanup(logCtx context.Context) {
    if c == nil {
        return
    }
    c.cleanupMu.Lock()
    if c.cleanedUp {
        c.cleanupMu.Unlock()
        return
    }

    // 先停 informer（幂等，可重复调用）
    if c.configMapStore != nil {
        c.configMapStore.Stop()
    }

    ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
    defer cancel()

    if !c.isDataPlane {
        err := c.cleanupControlPlane(logCtx, ctx)
        if err != nil {
            // 清理未完成，允许下次重试
            c.cleanupMu.Unlock()
            plog.G(logCtx).Warnf("Cleanup incomplete, will retry: %v", err)
            return
        }
    } else {
        c.cleanupDataPlane(logCtx)
    }
    c.cleanedUp = true
    c.cleanupMu.Unlock()
}
```

`cleanupControlPlane` 改为返回 error，这样调用方可以知道是否完成。

**改动文件**：`pkg/handler/cleaner.go`、`pkg/handler/connect.go`（字段定义）

---

### 2.4 gRPC TCP forwarder goroutine 泄漏

**现状**：`pkg/core/gvisor_tcp_forwarder.go:84-99` 启动两个 goroutine 做双向 copy，但只从 `errChan` 读一个错误。第二个 goroutine 的 `io.CopyBuffer` 可能阻塞在已关闭连接的 `Read` 上。

**修复方案**：两个 copy 完成后都通知，函数等两个都结束：

```go
errChan := make(chan error, 2)
go func() {
    buf := config.LPool.Get().([]byte)[:]
    defer config.LPool.Put(buf[:])
    _, err2 := io.CopyBuffer(remote, conn, buf)
    errChan <- err2
}()
go func() {
    buf := config.LPool.Get().([]byte)[:]
    defer config.LPool.Put(buf[:])
    _, err2 := io.CopyBuffer(conn, remote, buf)
    errChan <- err2
}()
// 等第一个完成后，关闭双方的读端以释放第二个
err = <-errChan
// 设置短超时触发第二个 goroutine 退出
conn.SetReadDeadline(time.Now().Add(time.Second))
remote.SetReadDeadline(time.Now().Add(time.Second))
<-errChan // 等第二个也完成
```

**改动文件**：`pkg/core/gvisor_tcp_forwarder.go`

---

### 2.5 port-forward 重连泄漏监控 goroutine

**现状**：`pkg/handler/network.go:409-417` 每次 `portForwardOnce` 创建 `childCtx` 并启动 `CheckPodStatus` 和 `healthCheckTCPConn` goroutine。`childCtx` 在 `portForwardOnce` 返回时取消（`defer cancelFunc()`），但 `healthCheckTCPConn` 中的 `grpc.DialContext` 用了 `WithBlock()` 可能在 context 取消后仍在阻塞。

**修复方案**：

1. `healthCheckTCPConn` 中的 gRPC 连接创建添加独立超时：

```go
// 修复前:
dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)

// 已经有 5s 超时，但要确保连接在 defer 中关闭
defer func() {
    if conn != nil {
        conn.Close()
    }
}()
```

实际上当前代码（`connect_tun.go:79`）已经在函数末尾 close conn。问题是 `grpc.DialContext` 的 `WithBlock()` 调用。如果 `childCtx` 在 dial 期间取消，`DialContext` 应该返回 error。验证确认 `grpc.DialContext` 尊重 context 取消。

真正的风险是 `conn.Close()` 只在循环结束后调用。如果 context 取消时 goroutine 正在 `GetTunIP` 调用中，gRPC 调用会因 context 取消而返回，然后循环退出。这路径已经正确。

**结论**：port-forward 重连的监控 goroutine 通过 `childCtx` 取消正确清理。实际风险低于审计报告的评估。仍然建议在 `healthCheckTCPConn` 开头添加 `defer` 确保 conn 关闭：

```go
func healthCheckTCPConn(...) {
    var conn *grpc.ClientConn
    defer func() {
        if conn != nil { conn.Close() }
    }()
    // ...
}
```

**改动文件**：`pkg/handler/connect_tun.go`

---

### 2.6 watchTunIPChanges 无 context 取消

**现状**：`pkg/core/protocol_registry.go:114-156` 的 `watchTunIPChanges` 使用 `context.Background()` 的无限循环，无法被外部停止。

**修复方案**：接受 `context.Context` 参数，循环检查 `ctx.Err()`：

```go
// 修复前:
func watchTunIPChanges(listener net.Listener) {
    ctx := context.Background()
    for {

// 修复后:
func watchTunIPChanges(ctx context.Context, listener net.Listener) {
    for ctx.Err() == nil {
```

所有 `grpc.DialContext` 和 `WatchTunIP` 调用传入 `ctx`，而不是 `context.Background()`。`pollTunIP` 也改为检查 `ctx.Err()`:

```go
func pollTunIP(ctx context.Context, ...) {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()
    for i := 0; i < 10; i++ {
        select {
        case <-ticker.C:
            // ...
        case <-ctx.Done():
            return
        }
    }
}
```

调用方（`tunProtocolFactory`）将服务器的 context 传入。

**改动文件**：`pkg/core/protocol_registry.go`

---

## 三、重要：安全

### 3.1 TLS 降级为明文 TCP

**现状**：`pkg/core/transporter_tcp.go:22-28`，当 TLS 配置加载失败（`ErrNoTLSConfig`）时，静默降级为 raw TCP。

**修复方案**：`ErrNoTLSConfig`（Secret 不存在）仍然允许降级（兼容无 TLS 的开发场景），但其他 TLS 错误（证书解析失败、密钥损坏）改为返回错误不降级：

```go
func TCPTransporter(tlsInfo map[string][]byte) Transporter {
    tlsConfig, err := util.GetTlsClientConfig(tlsInfo)
    if err != nil {
        if errors.Is(err, util.ErrNoTLSConfig) {
            plog.G(context.Background()).Warn("[Transport] No TLS config provided, using raw TCP")
            return &tcpTransporter{}
        }
        // TLS 配置存在但解析失败 — 不降级，返回一个会拒绝连接的 transporter
        plog.G(context.Background()).Errorf("[Transport] TLS config invalid: %v — connections will fail", err)
        return &failTransporter{err: fmt.Errorf("TLS config invalid: %w", err)}
    }
    return &tcpTransporter{tlsConfig: tlsConfig}
}

type failTransporter struct{ err error }
func (t *failTransporter) Dial(ctx context.Context, addr string) (net.Conn, error) {
    return nil, t.err
}
```

服务端（`gvisor_tcp_handler.go`）同样处理。

**改动文件**：`pkg/core/transporter_tcp.go`、`pkg/core/gvisor_tcp_handler.go`

---

### 3.2 VPN sidecar 去掉 Privileged

**现状**：`pkg/inject/container.go:42-49`，`privilegedSecurityContext()` 同时设置了 `Privileged: true` 和 `NET_ADMIN` capability。`Privileged: true` 给予容器所有 Linux capabilities + 访问宿主设备，远超 iptables 和 TUN 操作所需。

**修复方案**：去掉 `Privileged`，只保留所需 capabilities：

```go
func privilegedSecurityContext() *v1.SecurityContext {
    return &v1.SecurityContext{
        Capabilities: &v1.Capabilities{
            Add: []v1.Capability{"NET_ADMIN", "NET_RAW"},
        },
        RunAsUser:  ptr.To[int64](0),
        RunAsGroup: ptr.To[int64](0),
        // Privileged 不再设置（默认 false）
    }
}
```

`NET_RAW` 用于 ICMP 原始套接字（心跳包）。`NET_ADMIN` 用于 iptables 和 TUN 设备操作。不需要 `SYS_ADMIN` 或 `Privileged`。

**注意**：部分集群的 PSP/PSA 策略可能要求 `allowPrivilegeEscalation: false`，可以额外设置。

**改动文件**：`pkg/inject/container.go`

---

## 四、轻微问题

### 4.1 RouteHub WriteFuncToRoute TOCTOU

**现状**：`pkg/core/route.go:158-159`，先 `list.IsEmpty()` 检查，再 `hub.RouteMapTCP.Delete(dstKey)`。两步之间其他 goroutine 可能添加了新连接。

**修复方案**：在 `ConnList` 内部原子化检查-删除，或者用 `sync.Map.LoadAndDelete` 配合验证：

```go
// Write 失败后检查是否需要清理
if list.IsEmpty() {
    // 使用 CompareAndDelete（Go 1.20+）或 LoadAndDelete 后验证
    hub.RouteMapTCP.Delete(dstKey)
}
```

实际上 `sync.Map.Delete` 是幂等的，即使另一个 goroutine 又添加了条目，也不会丢数据（下一个包会重新注册路由）。影响极小。

**改动文件**：`pkg/core/route.go`（添加注释说明竞态无害即可）

---

### 4.2 Mapper informer AddEventHandler 错误忽略

**现状**：`pkg/handler/proxy_mapper.go:77`，`AddEventHandler` 的返回值被忽略。

**修复方案**：

```go
if _, err := m.cmInformer.AddEventHandler(...); err != nil {
    plog.G(m.ctx).Errorf("Failed to add ConfigMap event handler: %v", err)
    return
}
```

**改动文件**：`pkg/handler/proxy_mapper.go`

---

### 4.3 ReleaseIP 错误被忽略

**现状**：`tun_config.go:207` 和 `:451` 中 `_ = s.dhcp.ReleaseIP(...)`。

**修复方案**：记录日志（不影响主流程，best-effort 释放）：

```go
if err := s.dhcp.ReleaseIP(ctx, ipv4, ipv6); err != nil {
    plog.G(ctx).Warnf("[TunConfig] Failed to release IP %v: %v", ipv4, err)
}
```

**改动文件**：`pkg/controlplane/tun_config.go`

---

### 4.4 rollbackFuncList 无同步

**现状**：`pkg/handler/connect.go:316-317`，`AddRollbackFunc` 直接 append slice 无锁。虽然当前调用都在同一 goroutine，但 `Cleanup` 中的 `getRollbackFuncs` 在不同 goroutine 读取。

**修复方案**：添加 mutex 保护：

```go
type ConnectOptions struct {
    // ...
    rollbackMu       sync.Mutex
    rollbackFuncList []func() error
}

func (c *ConnectOptions) AddRollbackFunc(f func() error) {
    c.rollbackMu.Lock()
    defer c.rollbackMu.Unlock()
    c.rollbackFuncList = append(c.rollbackFuncList, f)
}

func (c *ConnectOptions) getRollbackFuncs() []func() error {
    c.rollbackMu.Lock()
    defer c.rollbackMu.Unlock()
    fns := make([]func() error, len(c.rollbackFuncList))
    copy(fns, c.rollbackFuncList)
    return fns
}
```

**改动文件**：`pkg/handler/connect.go`

---

### 4.5 连接池 4 slot 硬编码

**现状**：`pkg/core/tun_client.go:21`，`ConnPoolSize = 4` 硬编码。

**修复方案**：改为可通过环境变量覆盖的常量：

```go
var ConnPoolSize = func() int {
    if s := os.Getenv("KUBEVPN_CONN_POOL_SIZE"); s != "" {
        if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 16 {
            return n
        }
    }
    return 4
}()
```

**改动文件**：`pkg/core/tun_client.go`

---

### 4.6 Envoy header takeover 静默

**现状**：`addVirtualRule` Case 3（`pkg/inject/envoy.go`）当两个用户使用相同 headers 时，后者静默覆盖前者的路由规则，无任何日志或告警。

**修复方案**：添加 Warn 级日志：

```go
// Case 3: header takeover
plog.G(ctx).Warnf("[Envoy] Header takeover: owner %s is replacing owner %s for workload %s with headers %v",
    spec.OwnerID, existing.OwnerID, spec.NodeID, spec.Headers)
```

**改动文件**：`pkg/inject/envoy.go`

---

### 4.7 其他轻微改进

| # | 问题 | 修复 | 文件 |
|---|------|------|------|
| 4.7a | Mapper pod informer 未确保停止 | `podInformer.Run(m.ctx.Done())` 已通过 ctx 控制，无需改动 | 无 |
| 4.7b | OwnerID vs ConnectionID 边界 | 文档问题，补充说明即可 | `docs/04-connection-id.md` |
| 4.7c | channel buffer 内存压力 | `MaxSize=1000` × 64KB = 64MB 上限可接受，添加注释说明 | `pkg/core/tun_device.go` |
| 4.7d | NetworkManager 嵌入 clientset | 这是必要的（DNS、路由、port-forward 都需要 K8s API），无需改动 | 无 |
| 4.7e | 控制面 sidecar 无独立生命周期 | 设计决策，非 bug | 无 |

---

## 五、架构重构（中期）

以下为中期改进方向，不在本次修复范围内，但记录设计思路：

### 5.1 ConnectOptions 拆分

拆为 `ControlPlaneSession`（用户 daemon：K8s client、proxy 管理、健康检查）和 `DataPlaneSession`（root daemon：NetworkManager、TUN）。两者共享 `ConnectionIdentity`（OwnerID、ConnectionID、Namespace）。

### 5.2 handler 反向依赖 daemon/rpc

定义 `IPAllocator` 接口在 `pkg/handler`：

```go
type IPAllocator interface {
    RentIP(ctx context.Context, ownerID string, excludeIPs []net.IP) (v4, v6 *net.IPNet, err error)
}
```

daemon 层注入实现，消除 `handler → daemon/rpc` 导入。

### 5.3 Connection 接口完成迁移

`Server.connections` 改为 `[]Connection` 类型，daemon 层只通过接口方法操作。持久化通过接口方法 `MarshalConfig()`/`UnmarshalConfig()` 实现。

---

## 六、改动总结

| 优先级 | 改动 | 文件 | 风险 |
|--------|------|------|------|
| P0 | GetTunIP 不释放 mutex | `tun_config.go` | 低 |
| P0 | HealthStatus 加 RWMutex | `configmap_store.go` | 低 |
| P1 | saveAllocs 同步 + 记日志 | `tun_config.go` | 低 |
| P1 | reapExpiredLeases 二次检查 | `tun_config.go` | 低 |
| P1 | Cleanup 可重试 | `cleaner.go`, `connect.go` | 中 |
| P1 | TCP forwarder 等两个 goroutine | `gvisor_tcp_forwarder.go` | 低 |
| P1 | healthCheckTCPConn defer close | `connect_tun.go` | 低 |
| P1 | watchTunIPChanges 接受 ctx | `protocol_registry.go` | 低 |
| P1 | TLS 不降级（配置错误时） | `transporter_tcp.go` | 中 |
| P1 | 去掉 Privileged | `container.go` | 中（可能影响特定集群） |
| P2 | rollbackFuncList 加锁 | `connect.go` | 低 |
| P2 | ReleaseIP 记日志 | `tun_config.go` | 低 |
| P2 | Mapper AddEventHandler 检查 | `proxy_mapper.go` | 低 |
| P2 | ConnPoolSize 可配置 | `tun_client.go` | 低 |
| P2 | Header takeover 日志 | `envoy.go` | 低 |
| P2 | RouteHub TOCTOU 注释 | `route.go` | 低 |

## 七、验证方案

```bash
# 编译验证
go build ./...
go vet ./pkg/controlplane/... ./pkg/handler/... ./pkg/core/... ./pkg/inject/...

# 单元测试
go test ./pkg/controlplane/... -v -count=1
go test ./pkg/handler/... -v -count=1
go test ./pkg/core/... -v -count=1

# 竞态检测（需要 CGO_ENABLED=1 环境）
# go test -race ./pkg/controlplane/... ./pkg/handler/...

# 集成测试
go test ./pkg/controlplane/... -run Integration -v
go test ./pkg/inject/... -run Multiuser -v
```
