# Client 侧 TUN IP 热更新设计

## 1. 背景

当用户执行 `kubevpn connect` 或 `kubevpn proxy` 时，本地（client 侧）也会创建一个 TUN 设备。当前 IP 在 DHCP RentIP
后固化，整个连接生命周期内不可变更。

与 sidecar 侧一致，client 侧也需要支持 TUN IP 热更新。使用相同的 control-plane TunConfigService 机制。

## 2. Client 与 Sidecar 的差异

| 维度     | Sidecar                 | Client (本地)                             |
|--------|-------------------------|-----------------------------------------|
| 运行位置   | Pod 内                   | 用户本地机器 (root daemon)                    |
| 连接方式   | 直连 traffic-manager:9002 | port-forward → traffic-manager:9002     |
| TUN 管理 | protocol_registry.go    | NetworkManager (pkg/handler/network.go) |
| DHCP   | 通过 TunConfigService     | 当前: 直接操作 ConfigMap (user daemon)        |
| 生命周期   | Pod 生命周期                | 连接生命周期 (DoConnect → Cleanup)            |

**关键差异：** Client 的 TUN 由 NetworkManager 管理，它已有 Start/Stop 生命周期。而 sidecar 的 TUN 由 protocol_registry 的
tunProtocolFactory 创建。

## 3. 设计目标

- Client 侧复用相同的 `TunConfigService` gRPC API 获取和监听 IP
- NetworkManager 获得 `ChangeTunIP()` 方法，支持运行时修改
- 与 sidecar 使用相同的 `tun.ChangeIP` 底层能力
- heartbeat 已经支持动态读 IP（前序工作完成）
- 当 control-plane 推送 IP 变更时，NetworkManager 自动应用

## 4. 架构

```
User Machine
┌───────────────────────────────────────────────────┐
│  User Daemon (控制面)                              │
│  ├─ ConnectOptions                                │
│  │   ├─ DHCP (当前直接操作 ConfigMap)              │
│  │   ├─ LocalTunIPv4/v6                           │
│  │   └─ HealthCheck                              │
│  │                                                │
│  └─ → gRPC → Root Daemon                         │
│                                                    │
│  Root Daemon (数据面)                              │
│  ├─ ConnectOptions.DoConnect()                    │
│  │   └─ NetworkManager.Start()                   │
│  │       ├─ portForward (→ traffic-manager:10801) │
│  │       ├─ startTUN (创建 TUN 设备)              │
│  │       ├─ routes                                │
│  │       └─ DNS                                   │
│  │                                                │
│  └─ 新增: TunIPWatcher (后台)                     │
│       ├─ port-forward → traffic-manager:9002      │
│       ├─ WatchTunIP(ownerID=OwnerID(UUID))         │
│       └─ IP 变化 → NetworkManager.ChangeTunIP()   │
└───────────────────────────────────────────────────┘
         ↕ port-forward
┌───────────────────────────┐
│  Traffic Manager Pod       │
│  TunConfigService:9002     │
│  ├─ GetTunIP(ownerID)     │
│  └─ WatchTunIP(ownerID)   │
└───────────────────────────┘
```

## 5. 组件设计

### 5.1 NetworkManager 新增 `ChangeTunIP`

```go
// pkg/handler/network.go

// ChangeTunIP hot-updates the TUN device IP without restarting the network stack.
// Updates: OS interface address, iptables (if applicable), internal state.
// The next heartbeat automatically uses the new IP.
func (nm *NetworkManager) ChangeTunIP(ctx context.Context, newIPv4, newIPv6 *net.IPNet) error {
// 1. tun.ChangeIP on OS device
if err := tun.ChangeIP(nm.tunName, nm.cfg.LocalTunIPv4.String(), newIPv4.String()); err != nil {
return fmt.Errorf("change IPv4: %w", err)
}
if newIPv6 != nil && nm.cfg.LocalTunIPv6 != nil {
_ = tun.ChangeIP(nm.tunName, nm.cfg.LocalTunIPv6.String(), newIPv6.String())
}

// 2. Update iptables (no-op on client — client doesn't use DNAT)
// tun.UpdateDNAT(nm.cfg.LocalTunIPv4.IP, newIPv4.IP)  // client doesn't need this

// 3. Update internal config
nm.cfg.LocalTunIPv4 = newIPv4
nm.cfg.LocalTunIPv6 = newIPv6

plog.G(ctx).Infof("[NetworkManager] TUN IP changed: v4=%s v6=%s", newIPv4, newIPv6)
return nil
}
```

### 5.2 `rentIP` 冲突避免 — ExcludeIPs 方案

`NetworkManager.rentIP` 将本地所有接口 IP 作为 `ExcludeIPs` 传给 server，server 在 DHCP 分配时
直接跳过这些 IP。通常一次调用即可拿到不冲突的 IP。

保留轻量重试处理非原子竞态（采集本地 IP 和分配之间可能出现新接口）：

```go
const maxRetries = 15
for i := 0; i < maxRetries; i++ {
    resp, err := client.GetTunIP(ctx, &rpc.TunIPRequest{
        OwnerID:    nm.cfg.OwnerID,
        Namespace:  nm.cfg.ManagerNamespace,
        ExcludeIPs: collectLocalIPs(),  // 每次重新采集
    })
    v4 := parse(resp.IPv4)
    if !isLocalIPConflict(v4.IP) {
        return nil  // 成功
    }
    // 极罕见竞态：采集和分配之间出现了新接口
}
```

**Server 端处理：**
- 已有分配且不在 ExcludeIPs 中 → 正常返回（续租）
- 已有分配但在 ExcludeIPs 中 → 旧 IP 保留在 bitmap，调用 `dhcp.RentIPExcluding(excludeIPs)` 分配新 IP（自然跳过旧 IP），然后释放旧 IP
- 无分配 → `dhcp.RentIPExcluding(excludeIPs)` 新分配

**为什么不用 ForceNew（先 release 再 rent）：** DHCP 使用 `contiguousScanStrategy`，从 offset 0
顺序扫描。release 后 rent 总是拿回同一个 IP，多轮重试会死循环。ExcludeIPs 方案在单次 DHCP 事务中
跳过冲突 IP，避免了这个问题。

### 5.3 TunIPWatcher for Client (在 Root Daemon 运行)

Root daemon 中 `DoConnect` 完成后，启动一个后台 goroutine 监听 TunConfigService：

```go
// pkg/handler/network.go 或 pkg/handler/tun_watcher.go

// StartIPWatcher connects to the control-plane's TunConfigService via port-forward
// and watches for IP changes. When a change is detected, it calls ChangeTunIP.
func (nm *NetworkManager) StartIPWatcher(ctx context.Context, ownerID string) {
go nm.watchTunIPFromControlPlane(ctx, ownerID)
}

func (nm *NetworkManager) watchTunIPFromControlPlane(ctx context.Context, ownerID string) {
// traffic manager 的 9002 端口通过 port-forward 暴露在本地
// 或者通过 K8s service 直接访问（如果有 kubeconfig 权限）
target := fmt.Sprintf("127.0.0.1:%d", controlPlaneLocalPort)

var currentVersion int64
for ctx.Err() == nil {
conn, err := grpc.DialContext(ctx, target, grpc.WithInsecure())
if err != nil {
time.Sleep(10 * time.Second)
continue
}
client := rpc.NewTunConfigServiceClient(conn)

stream, err := client.WatchTunIP(ctx, &rpc.TunIPRequest{
OwnerID:   ownerID,
Namespace: nm.cfg.ManagerNamespace,
})
if err != nil {
conn.Close()
time.Sleep(30 * time.Second)
continue
}

for {
resp, err := stream.Recv()
if err != nil {
break
}
if resp.Version != currentVersion && currentVersion != 0 {
newV4, newV6 := parseIPResponse(resp)
nm.ChangeTunIP(ctx, newV4, newV6)
}
currentVersion = resp.Version
}
conn.Close()
time.Sleep(5 * time.Second)
}
}
```

### 5.4 OwnerID 选择

Sidecar 使用 `podName` 作为 ownerID。Client 使用 **OwnerID**（UUID，每次连接时生成）：

```go
ownerID := c.OwnerID // uuid.New().String()[:12]，在 connect_elevate.go 创建
```

**为什么不用 connectionID：**

- connectionID = namespace UID 后 12 位，是 **per-namespace** 的标识
- 两个用户连接同一 namespace 时 connectionID 相同，TunConfigService 无法区分
- 断连重连时 connectionID 不变，无法区分新旧会话

**OwnerID 的优势：**

- 每次 connect 生成新 UUID（全局唯一）
- 在 user daemon 创建时即可用（早于 DHCP 和 TUN）
- 已用于 envoy rule 所有权追踪（`controlplane.Rule.OwnerID`）
- 持久化到 daemon config（`json:"OwnerID"`），重启恢复时使用相同 ID
- Root Daemon 需要通过 gRPC metadata 接收此值（当前只传 IP，需新增 OwnerID 传递）

### 5.5 Port-Forward 到 Control-Plane 9002

当前 NetworkManager 已经 port-forward 到 traffic-manager 的 10801/10802 端口（gvisor TCP/UDP）。需要额外 port-forward 9002
端口用于 TunConfigService gRPC。

两种方案：

**方案 A: 复用现有 port-forward 机制**
在 NetworkManager.Start 中额外转发 9002：

```go
portPair := []string{
fmt.Sprintf("%d:10801", gvisorTCPPort),
fmt.Sprintf("%d:10802", gvisorUDPPort),
fmt.Sprintf("%d:9002", localControlPlanePort), // 新增
}
```

**方案 B: 独立 port-forward**
单独一个 goroutine 专门 port-forward 9002，生命周期与 watcher 绑定。

推荐 **方案 A** — 更简单，复用已有机制。

### 5.6 ConnectOptions 中 DHCP 路径的变更

**当前流程 (User Daemon):**

```
RentIP → 直接操作 ConfigMap DHCP → 获得 IP
→ 将 IP 传给 Root Daemon (via gRPC metadata)
→ Root Daemon 用 IP 创建 TUN
```

**新流程 (使用 TunConfigService):**

```
User Daemon: GetTunIP(ownerID) via port-forward → control-plane
→ 获得 IP → 传给 Root Daemon (via gRPC metadata)
→ Root Daemon 用 IP 创建 TUN
→ Root Daemon 启动 WatchTunIP → IP 变更时 ChangeTunIP
```

注意：User Daemon 仍然做初始 IP 获取（因为它需要在 Root Daemon 启动 TUN 之前就知道 IP，以通过 gRPC metadata 传递）。但后续的
IP 变更由 Root Daemon 的 watcher 处理。

### 5.7 User Daemon 健康检查适配

User Daemon 的 `HealthCheckOnce` 从 ConfigMap 读取状态。如果 IP 变了，User Daemon 也需要更新 `LocalTunIPv4/v6` 字段（用于
status 显示和 LeaveResource 时的 IP 匹配）。

通过 User Daemon 也监听 TunConfigService 即可同步：

```go
// connect_elevate.go — forwardConnectToSudo 完成后
go syncTunIPFromControlPlane(ctx, connect, ownerID)
```

## 6. 完整生命周期

```
kubevpn connect -n default
  │
  ├─ User Daemon:
  │   1. 创建 ConnectOptions
  │   2. InitClient → configMapStore
  │   3. GetTunIP(ownerID=OwnerID(UUID)) via port-forward:9002
  │      → control-plane DHCP rent → 198.18.0.5/32
  │   4. RentIP → 将 IP 设置到 ConnectOptions
  │   5. forwardConnectToSudo → 传 IP 给 Root Daemon
  │   6. HealthPeriod (后台)
  │   7. WatchTunIP (后台) → 同步 LocalTunIPv4 显示
  │
  ├─ Root Daemon:
  │   1. GetOwnerIDFromContext → 获取 OwnerID
  │   2. DoConnect → NetworkManager.Start()
  │      → portForward(:10801, :10802, :9002)
  │      → rentIP: GetTunIP(ownerID, ExcludeIPs=localIPs)
  │        → server 跳过冲突 IP → 198.18.0.6/32
  │        → isLocalIPConflict? → no → 使用此 IP
  │      → startTUN(198.18.0.6)
  │      → routes, DNS
  │   3. StartIPWatcher(ownerID) (后台)
  │      → WatchTunIP via localhost:localPort → 9002
  │
  ├─ IP 变更场景:
  │   外部: control-plane 决定重新分配 IP
  │   → WatchTunIP stream push: 198.18.0.10/32
  │   → Root Daemon: NetworkManager.ChangeTunIP()
  │      → tun.ChangeIP("utun0", old, new)
  │      → heartbeat 自动用新 IP
  │      → Server RouteHub auto-register
  │   → User Daemon: 同步 LocalTunIPv4 (for status/leave)
  │
  └─ Disconnect:
      → Lease 到期自动回收 (无需显式释放)
      → Root Daemon: NetworkManager.Stop()
```

## 7. 与 Sidecar 实现的对比

| 步骤       | Sidecar                               | Client                                     |
|----------|---------------------------------------|--------------------------------------------|
| 获取 IP    | `tunProtocolFactory` → gRPC dial 9002 | User Daemon → port-forward:9002 → GetTunIP |
| 创建 TUN   | `tun.Listener` in protocol_registry   | `NetworkManager.startTUN`                  |
| 监听变化     | `watchTunIPChanges` goroutine         | `NetworkManager.StartIPWatcher`            |
| 应用变更     | `applyTunIPChange` (直接操作 OS)          | `NetworkManager.ChangeTunIP`               |
| iptables | UpdateDNAT (VPN-only 模式)              | 不需要 (client 不做 DNAT)                       |
| 释放 IP    | 无需 — lease 过期自动回收              | 无需 — lease 过期自动回收                    |
| ownerID  | podName                               | OwnerID (UUID, 每次连接唯一)                  |

## 8. 复用的代码

| 组件                       | 包                        | Sidecar 和 Client 共用 |
|--------------------------|--------------------------|---------------------|
| `tun.ChangeIP`           | `pkg/tun`                | ✅                   |
| `tun.UpdateDNAT`         | `pkg/tun`                | Sidecar only        |
| heartbeat 动态读 IP         | `pkg/core/tun_client.go` | ✅                   |
| `TunConfigService` proto | `pkg/daemon/rpc`         | ✅ (同一 gRPC API)     |
| `TunConfigServer`        | `pkg/controlplane`       | ✅ (server 端同一实例)    |
| gRPC client 逻辑           | 各自实现                     | 类似但适配不同连接方式         |

## 9. 文件变更

| 文件                                     | 操作 | 说明                                 |
|----------------------------------------|----|------------------------------------|
| `pkg/handler/network.go`               | 修改 | 新增 `ChangeTunIP`, `StartIPWatcher` |
| `pkg/handler/connect.go`               | 修改 | DoConnect 完成后启动 watcher            |
| `pkg/daemon/action/connect_elevate.go` | 修改 | User Daemon 用 GetTunIP 替代直接 DHCP   |
| `pkg/daemon/action/connect.go`         | 修改 | Root Daemon 启动 IP watcher          |

## 10. 向后兼容

- 如果 TunConfigService 不可用（旧版 traffic manager），退回直接 DHCP 模式
- Root Daemon 的 watcher 连接失败不影响 TUN 正常工作（只是不支持热更新）
- 新旧 client 可以连接同一个 traffic manager（旧 client 不调用 TunConfigService，通过 ConfigMap DHCP 分配 IP）

## 11. 已知问题修复

### WatchTunIP 长连接 Lease 过期（已修复）

原始设计中 `WatchTunIP` stream 不刷新 `LastRenew`，超过 5 分钟后 LeaseReaper 会错误回收 IP。
已在 server 端 `WatchTunIP` 中加入隐式续租 ticker（每 `LeaseDuration/3` 刷新一次）。
详见 [23-watchtunip-lease-renewal-fix.md](23-watchtunip-lease-renewal-fix.md)。
