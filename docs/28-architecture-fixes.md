# Architecture Issue Fixes

## 概述

本文档详述对 KubeVPN 架构审查中发现的问题的修复方案。排除了已确认不修的项目（SSH 认证、控制面认证、SPOF、ConfigMap 替换、Socket 权限、mTLS、TLS 私钥暴露、IP 限流、daemon 间授权、线协议长度、镜像 TLS、xDS 流数限制），剩余需修复的问题分为 4 类。

---

## 一、严重：并发安全

### 1.1 GetTunIP mutex 释放窗口导致重复 IP 分配

> **状态：✅ 已修复。** `GetTunIP` 现在使用 `s.mu.Lock()` + `defer s.mu.Unlock()` 保护整个方法，`RentIPExcluding` 在锁内直接调用，不再有 unlock/relock 窗口。注释 "Hold mutex across RentIP" 明确记录了设计意图。

**原始问题（仅作历史记录）**：`pkg/controlplane/tun_config.go` 在调用 `dhcp.RentIPExcluding` 前手动 `s.mu.Unlock()`，完成后再 `s.mu.Lock()`。两个并发 `GetTunIP` 可能在窗口内拿到同一 IP。

---

### 1.2 HealthStatus 跨 goroutine 读写无同步

> **状态：✅ 已修复。** `ConfigMapStore` 添加了 `healthMu sync.RWMutex`，`syncFromCache` 和 `HealthCheckOnce` 写入时使用 `Lock`，`GetHealthStatus` 读取时使用 `RLock`。

**原始问题（仅作历史记录）**：`syncFromCache` 和 `HealthCheckOnce` 在后台 goroutine 写 `healthStatus`，`GetHealthStatus` 在主 goroutine 读取，无锁保护。

---

## 二、重要：并发与可靠性

### 2.1 DHCP bitmap 与 TUN_ALLOCS 非原子更新 + saveAllocs 异步吞错误

> **状态：✅ 已修复。** `saveAllocs` 现在同步调用且返回 error，所有调用点（`loadAllocs`、`GetTunIP` 两处、`reapExpiredLeases`）都检查并记录错误。`GetTunIP` 在 mutex 保护下依次调用 `RentIPExcluding` → `saveAllocs`，崩溃窗口已最小化。

**原始问题（仅作历史记录）**：`saveAllocs` 在单独 goroutine 中异步执行且错误被忽略（`_ = retry.RetryOnConflict(...)`），导致 DHCP bitmap 和 TUN_ALLOCS 不一致时 IP 永久泄漏。

---

### 2.2 reapExpiredLeases 竞态窗口

> **状态：✅ 已修复。** `reapExpiredLeases` 重新获取锁后执行二次检查 `if !ok || time.Since(alloc.LastRenew) <= LeaseDuration`，避免误删重连用户的条目。

**原始问题（仅作历史记录）**：先收集过期 ownerID，释放锁，再逐个重新加锁删除。窗口内客户端可能已重连并分配了新条目，导致新条目被误删。

---

### 2.3 Cleanup sync.Once 不可重试

> **状态：✅ 已修复。** `sync.Once` 替换为 `cleanupMu sync.Mutex` + `cleanedUp bool`。`cleanupControlPlane` 改为返回 error，失败时释放锁允许下次重试。

**原始问题（仅作历史记录）**：`sync.Once` 包裹所有清理逻辑，10 秒超时内未完成的步骤永远不会重试。

---

### 2.4 gRPC TCP forwarder goroutine 泄漏

> **状态：✅ 已修复。** 第一个 goroutine 完成后设置双方 `SetReadDeadline(1s)`，然后 `<-errChan` 等待第二个 goroutine 也完成，消除泄漏。

**原始问题（仅作历史记录）**：启动两个 goroutine 做双向 copy，但只从 `errChan` 读一个错误。第二个 goroutine 的 `io.CopyBuffer` 可能阻塞在已关闭连接的 `Read` 上。

---

### 2.5 port-forward 重连泄漏监控 goroutine

> **状态：✅ 已修复（重写）。** `healthCheckTCPConn` 已被 `healthCheckPortForward` 替代。新实现通过 gudp relay 发送 DNS 查询验证数据面存活，不再使用 gRPC。连接复用模式：TCP conn 和 PacketConn 在 checker 闭包外持有，失败时 `closeConn()` 清理并置 nil，下次 check 重建。函数末尾 `defer closeConn()` 确保资源释放。原先 gRPC conn 不用 defer 关闭的泄漏风险已消除。

---

### 2.6 watchTunIPChanges 无 context 取消（已废弃 — 函数已删除）

> **状态：已被统一代理模式取代，无需修复。** `watchTunIPChanges` / `pollTunIP` / `applyTunIPChange` 这套 sidecar 端的 TUN IP 热更新逻辑已在「统一代理模式」中**整体删除**（见 [29-sleep-wake-ip-update.md](29-sleep-wake-ip-update.md)）。sidecar 不再监听自身 IP 变化；客户端 IP 变化通过 envoy xDS（`syncEnvoyRuleIP`）下发。因此原先「无 context 取消」的问题随函数删除一并消失。

**原始问题（仅作历史记录）**：旧版 `pkg/core/protocol_registry.go` 的 `watchTunIPChanges` 使用 `context.Background()` 的无限循环，无法被外部停止。`tunProtocolFactory` 现在只在启动时调用一次 `requestTunIPFromControlPlane` 获取 IP，不再启动任何后台 watcher goroutine。

---

## 三、重要：安全

### 3.1 TLS 降级为明文 TCP

> **状态：✅ 已修复（客户端 + 服务端）。**
> - **服务端**（`gvisor_tcp_handler.go:GvisorTCPListener`）：非 `ErrNoTLSConfig` 错误时关闭 listener 并返回 error，不降级。
> - **客户端**（`transporter_tcp.go:TCPTransporter`）：非 `ErrNoTLSConfig` 错误返回 `failTransporter`，Dial 时返回错误，不降级为明文。

**原始问题（仅作历史记录）**：客户端在 TLS 配置解析失败时静默降级为 raw TCP。

---

### 3.2 VPN sidecar 去掉 Privileged

> **状态：✅ 已修复。** `Privileged: true` 已移除，只保留 `NET_ADMIN` + `NET_RAW` capabilities。`RunAsUser/RunAsGroup: 0` 保留。

**原始问题（仅作历史记录）**：`privilegedSecurityContext()` 同时设置了 `Privileged: true` 和 `NET_ADMIN` capability，给予容器所有 Linux capabilities + 访问宿主设备，远超所需。

---

## 四、轻微问题

### 4.1 RouteHub WriteFuncToRoute TOCTOU

> **状态：✅ 已修复。** 在 `WriteToRoute` 和 `WriteFuncToRoute` 的 `IsEmpty()` + `Delete()` 位置添加了注释，说明 TOCTOU 竞态无害（`sync.Map.Delete` 幂等，下一个包会通过 `AddRoute` 重新注册路由）。

**原始问题（仅作历史记录）**：先 `list.IsEmpty()` 检查再 `Delete`，两步之间其他 goroutine 可能添加了新连接。影响极小。

---

### 4.2 Mapper informer AddEventHandler 错误忽略

> **状态：✅ 已修复。** ConfigMap 和 Pod 两个 `AddEventHandler` 调用都检查返回值，失败时记录错误并 return。

**原始问题（仅作历史记录）**：`AddEventHandler` 的返回值被忽略。

---

### 4.3 ReleaseIP 错误被忽略

> **状态：✅ 已修复。** 所有三处 `ReleaseIP` 调用（`GetTunIP` 重分配、`loadAllocs` 过期释放、`reapExpiredLeases` 过期释放）都检查并记录错误。

**原始问题（仅作历史记录）**：`_ = s.dhcp.ReleaseIP(...)` 忽略错误。

---

### 4.4 rollbackFuncList 无同步

> **状态：✅ 已修复。** `rollbackMu sync.Mutex` 保护 `AddRollbackFunc`（写）和 `getRollbackFuncs`（读 + copy），消除跨 goroutine 竞态。

**原始问题（仅作历史记录）**：`AddRollbackFunc` 直接 append slice 无锁，`Cleanup` 中的 `getRollbackFuncs` 在不同 goroutine 读取。

---

### 4.5 连接池 4 slot 硬编码

> **状态：✅ 已修复。** `ConnPoolSize` 改为 `var`，支持 `KUBEVPN_CONN_POOL_SIZE` 环境变量覆盖（范围 1-16，默认 4）。

**原始问题（仅作历史记录）**：`ConnPoolSize = 4` 硬编码为 `const`，无法配置。

---

### 4.6 Envoy header takeover 静默

> **状态：✅ 已修复。** `addVirtualRule` Case 3 添加了 `Warn` 级日志，记录 takeover 的双方 ownerID、workload 和 headers。`addVirtualRule` 增加 `ctx` 参数以支持日志。

**原始问题（仅作历史记录）**：当两个用户使用相同 headers 时，后者静默覆盖前者的路由规则，无任何日志或告警。

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

> **最后审查时间：2026-06-11**

| 优先级 | 改动 | 文件 | 风险 | 状态 |
|--------|------|------|------|------|
| P0 | GetTunIP 不释放 mutex | `tun_config.go` | 低 | ✅ 已修复 |
| P0 | HealthStatus 加 RWMutex | `configmap_store.go` | 低 | ✅ 已修复 |
| P1 | saveAllocs 同步 + 记日志 | `tun_config.go` | 低 | ✅ 已修复 |
| P1 | reapExpiredLeases 二次检查 | `tun_config.go` | 低 | ✅ 已修复 |
| P1 | Cleanup 可重试 | `cleaner.go`, `connect.go` | 中 | ✅ 已修复 |
| P1 | TCP forwarder 等两个 goroutine | `gvisor_tcp_forwarder.go` | 低 | ✅ 已修复 |
| P1 | healthCheckPortForward 替代 gRPC | `connect_tun.go` | 低 | ✅ 已修复（重写） |
| ~~P1~~ | ~~watchTunIPChanges 接受 ctx~~（函数已删除，废弃） | `protocol_registry.go` | — | ✅ 已废弃 |
| P1 | TLS 不降级（配置错误时） | `transporter_tcp.go` | 中 | ✅ 已修复 |
| P1 | 去掉 Privileged | `container.go` | 中（可能影响特定集群） | ✅ 已修复 |
| P2 | rollbackFuncList 加锁 | `connect.go` | 低 | ✅ 已修复 |
| P2 | ReleaseIP 记日志 | `tun_config.go` | 低 | ✅ 已修复 |
| P2 | Mapper AddEventHandler 检查 | `proxy_mapper.go` | 低 | ✅ 已修复 |
| P2 | ConnPoolSize 可配置 | `tun_client.go` | 低 | ✅ 已修复 |
| P2 | Header takeover 日志 | `envoy.go` | 低 | ✅ 已修复 |
| P2 | RouteHub TOCTOU 注释 | `route.go` | 低 | ✅ 已修复 |

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
