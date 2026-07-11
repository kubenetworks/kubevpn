# Server 侧 TUN Device 热更新分析

## Server 侧架构总览

Traffic manager pod（server 侧）不直接使用 TUN 设备。它监听两个 TCP 端口：

```
Traffic Manager Pod
├── gtcp://:10801  → GvisorTCPHandler  → gvisor user-space TCP/IP stack
└── gudp://:10802  → GvisorUDPHandler  → UDP relay
```

Client 侧（用户本地）：
```
Client (本地)
├── TUN device (utun*)      ← 本地创建的虚拟网卡
├── gvisor stack            ← 处理 TUN 收到的 IP 包
└── TCP connection to :10801/:10802  ← 端口转发到 traffic manager
```

**关键发现：server 侧没有 TUN 设备。** 所有 IP 包通过 gvisor TCP/UDP 连接在 client-server 之间传输。Server 侧通过 gvisor stack 在 Pod 网络中收发包。

## 数据流

```
                Client                          Server (traffic manager pod)
                ─────                           ────────────────────────────
App → TUN device → gvisor stack → TCP conn →→→ GvisorTCPHandler → gvisor stack → Pod network
                                                        │
                                                   RouteHub (IP → conn mapping)
                                                        │
App ← TUN device ← gvisor stack ← TCP conn ←←← response back via same conn
```

## "修改 TUN IP" 的含义

### Client 侧 TUN IP

每个 VPN 客户端通过 DHCP 从 ConfigMap 获得一个唯一的 TUN IP（如 `198.18.0.5/32`）。这个 IP 用于：

1. **本地 TUN 设备地址** — 配置在 utun* 接口上
2. **路由标识** — server 侧 RouteHub 通过 src IP 识别客户端
3. **心跳包源 IP** — gvisor heartbeat 使用此 IP

### Server 侧无 TUN

Server 侧不创建 TUN 设备。它通过 gvisor stack（运行在 Pod 网络中）直接转发 IP 包。不存在 "修改 server TUN IP" 的场景。

## 热更新可行性分析

### 场景 1：修改 Client TUN IP（Server 侧影响）

如果 client 想更换 TUN IP（例如 IP 冲突、DHCP 续约失败）：

**Server 侧需要的变更：**
- `RouteHub` 中 old IP → conn 的映射需要更新为 new IP → conn
- 无需重启 gvisor stack（它不关心 client 的 src IP，只转发）
- ConfigMap 中的 DHCP 记录需更新

**可行性：高。** RouteHub 是 `sync.Map`，支持并发修改：
```go
// 1. Client 发送 "IP 变更" 通知
// 2. Server 从 RouteHub 删除旧 IP 映射
hub.RouteMapTCP.Delete(string(oldIP))
// 3. 下一个从新 IP 来的包自动通过 AddRoute 注册
hub.AddRoute(ctx, newIP, conn)
```

**结论：Server 侧无需任何结构性修改。** RouteHub 的 AddRoute 已经是幂等的（每个包都调用），当 client 使用新 IP 发送第一个包时，自动注册新路由。

### 场景 2：Server Pod 自身 IP 变化

Traffic manager pod 被重新调度时 Pod IP 会变：

**当前处理方式：** Client 的 `portForwardOnce` 有重试逻辑 — 检测到 pod 删除后重新选择新 pod 建立 port-forward。

**热更新策略：** 无需修改 server 侧代码。Client 的 NetworkManager.portForward 已经有自动重连。

### 场景 3：Client TUN IP 热切换（完整流程设计）

```
1. Client: 旧 IP lease 过期自动回收 (无需显式 release)
2. Client: DHCP rent new IP
3. Client: 修改本地 TUN 设备 IP (ip addr replace)
4. Client: 更新路由表
5. Client: 用新 IP 发送第一个包 → Server 自动注册新路由
6. Server: 旧 IP 映射在 RemoveRoutesByConn 中因连接关闭自动清理
         或者在超时后自然过期
```

## 技术约束

### 1. 本地 TUN IP 修改 — 操作系统层

| 平台 | 修改 IP 方式 | 需要重建 TUN? |
|------|-------------|---------------|
| Linux | `netlink.NetworkLinkDelIp` + `NetworkLinkAddIp` | **不需要** |
| macOS | `ioctl(SIOCDIFADDR)` + `ioctl(SIOCAIFADDR)` | **不需要** |
| Windows | `netsh interface ip set address` | **不需要** |

所有平台都支持在不销毁 TUN 设备的情况下修改 IP 地址。

### 2. gvisor Stack — Client 侧

当前 client 侧的 gvisor stack 在 `startTUN` 时创建，IP 硬编码在 stack 配置中。修改 TUN IP 后需要：
- 更新 gvisor stack 的 NIC 地址（或重建 stack）
- 当前实现中 stack 通过 `core.NewLocalStack` / `core.NewStack` 创建，IP 通过 heartbeat 注册

实际上 gvisor stack 只是一个中间转发层，它不关心本机 IP 地址 — 它转发的是完整 IP 包（src/dst 由应用层决定）。所以 **gvisor stack 不需要修改**。

### 3. RouteHub — Server 侧

RouteHub 的设计已经天然支持 IP 变更：
```go
func (hub *RouteHub) AddRoute(ctx, srcIP, conn) {
    // 每个包都会调用，用 srcIP 注册 conn
    // 如果 client 换了 IP，新 IP 的第一个包自动注册
}
```

旧 IP 的映射通过以下方式清理：
- 连接关闭时 `RemoveRoutesByConn` 删除所有该 conn 的映射
- 如果连接复用（同一 TCP 连接换 IP），旧 IP 映射不会自动清理但也不会造成问题（ConnList 中的 conn 相同）

### 4. DHCP 原子切换

```go
// pkg/dhcp/dhcp.go
// 无需显式 release — lease 过期自动回收
// manager.ReleaseIP(ctx, oldIPv4, oldIPv6)  // removed
manager.RentIP(ctx) // 获取新 IP
```

DHCP 操作通过 ConfigMap patch 实现，天然原子。

## 结论

| 组件 | 是否支持热更新 | 需要修改 |
|------|---------------|----------|
| Server RouteHub | ✅ 天然支持 | 无需修改 |
| Server gvisor stack | ✅ 不受影响 | 无需修改 |
| Client TUN IP | ✅ 操作系统支持 | 需要添加 `changeTunIP()` 方法 |
| Client gvisor stack | ✅ 不关心本机 IP | 无需修改 |
| Client DHCP | ✅ ConfigMap 原子操作 | 需要 release+rent 逻辑 |
| Client 路由表 | ⚠️ 需要刷新 | 需要更新路由中的 src IP |
| Client DNS 配置 | ✅ 不涉及 TUN IP | 无需修改 |

**总结：Server 侧完全不需要修改。** 热更新 TUN IP 是一个纯 Client 侧操作，需要在 NetworkManager 上添加一个 `ChangeTunIP(newIPv4, newIPv6)` 方法：

```go
func (nm *NetworkManager) ChangeTunIP(ctx context.Context, newIPv4, newIPv6 *net.IPNet) error {
    // 1. 修改 OS 层 TUN 设备 IP（不销毁设备）
    // 2. 更新路由表中与旧 IP 相关的规则
    // 3. 更新 nm.localTunIPv4/v6
    // 4. 新包自动用新 IP 发送，Server RouteHub 自动注册
}
```

这与 NetworkManager 的 `Start()/Stop()` 生命周期互补 — `ChangeTunIP` 是一个轻量级操作，不需要重建整个网络栈。
