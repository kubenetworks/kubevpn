# DHCP IP 分配设计文档

## 1. 概述

KubeVPN 使用自定义 DHCP 机制为每个 VPN 连接分配唯一的 TUN 设备 IP 地址。该机制不依赖传统的 DHCP 服务器，而是将 **Kubernetes ConfigMap 作为分布式存储**，配合 **bitmap 位图分配器** 实现无状态、无冲突的 IP 分配。

### 设计目标

- 多用户并发安全（通过 K8s 乐观锁 `RetryOnConflict`）
- 无需独立 DHCP 服务器进程
- 支持 IPv4 + IPv6 双栈
- IP 自动回收（lease 过期机制）
- 集群重启后 IP 分配状态可恢复

## 2. IP 地址池

| 协议 | CIDR | 地址数量 | 用途 |
|------|------|---------|------|
| IPv4 | `198.18.0.0/16` | 65,534 | TUN 设备 IP（IANA 保留用于基准测试） |
| IPv6 | `2001:2::/64` | ~2^64 | TUN 设备 IPv6 地址（IANA 保留用于基准测试） |
| Docker IPv4 | `198.19.0.0/16` | 65,534 | kubevpn run Docker 桥接网络 |

`198.18.0.0/15` 由 IANA 保留用于网络基准测试（RFC 2544），不与真实网络冲突。KubeVPN 将其拆分为两个 /16：`198.18.0.0/16` 用于 TUN，`198.19.0.0/16` 用于 Docker。

## 3. 架构

```
┌──────────────────────────────────────────────────────────┐
│                    Traffic Manager Pod                     │
│                                                           │
│  TunConfigServer (:9002 gRPC)                             │
│  ├── allocs map[ownerID]*tunAllocation  ← 内存缓存        │
│  ├── GetTunIP(ownerID, excludeIPs)      ← 分配/续租       │
│  ├── WatchTunIP(ownerID)                ← 推送 IP 变更    │
│  ├── LeaseReaper (30s tick)             ← 回收过期 IP     │
│  └── dhcp.Manager                       ← bitmap 分配器   │
│       ├── RentIP / RentIPExcluding                        │
│       ├── ReleaseIP                                       │
│       └── ForEach                                         │
│                                                           │
│  ConfigMap: kubevpn-traffic-manager                        │
│  ├── DHCP        = base64(bitmap-v4)    ← IPv4 分配位图   │
│  ├── DHCP6       = base64(bitmap-v6)    ← IPv6 分配位图   │
│  ├── TUN_ALLOCS  = json(ownerID→IP)     ← 持久化分配映射  │
│  └── ENVOY_CONFIG = yaml([]*Virtual)    ← envoy 路由规则  │
└──────────────────────────────────────────────────────────┘
         ↑ port-forward
┌─────────────────────┐
│  Root Daemon         │
│  NetworkManager      │
│  └── rentIP()        │
│       → GetTunIP()   │
└─────────────────────┘
```

## 4. 核心组件

### 4.1 dhcp.Manager（底层分配器）

`pkg/dhcp/dhcp.go` — 封装 cilium/ipam 的 bitmap 分配器。

**职责：** 在 ConfigMap 上执行原子 read-modify-write 操作来分配/释放 IP。

```
RentIPExcluding(ctx, excludeIPs):
  1. GET ConfigMap
  2. base64 decode → bitmap → ipallocator.Range
  3. AllocateNext() 循环直到找到不在 excludeIPs 中的 IP
  4. Snapshot() → base64 encode → UPDATE ConfigMap
  (冲突时 RetryOnConflict 自动重试)
```

**关键方法：**

| 方法 | 功能 |
|------|------|
| `RentIP(ctx)` | 分配下一个可用 IPv4 + IPv6 |
| `RentIPExcluding(ctx, excludeIPs)` | 分配 IP，跳过 excludeIPs 列表中的地址 |
| `ReleaseIP(ctx, v4, v6)` | 释放指定 IP 回池 |
| `ForEach(ctx, fnv4, fnv6)` | 遍历所有已分配 IP |
| `InitDHCP(ctx)` | 确保 ConfigMap 存在 |

**位图分配器 (cilium/ipam)：**

使用 `ContiguousAllocationMap` — 从 offset 0 开始顺序扫描位图，找到第一个空闲位。特性：
- 分配确定性（总是返回最小可用 IP）
- `Snapshot()` / `Restore()` 支持序列化为 `[]byte`
- 分配和释放的时间复杂度为 O(n)，n 为地址池大小

**并发安全：**

通过 K8s 的 `resourceVersion` 实现乐观锁：
```
GET ConfigMap (resourceVersion=X)
→ 修改 bitmap
→ UPDATE ConfigMap (携带 resourceVersion=X)
→ 冲突 (409 Conflict) → retry.RetryOnConflict 自动重试
```

### 4.2 TunConfigServer（控制面）

`pkg/controlplane/tun_config.go` — 在 Traffic Manager Pod 中运行的 gRPC 服务。

**职责：** 管理 ownerID → IP 的映射、lease 续租、IP 变更推送。

**内存状态：**
```go
allocs map[string]*tunAllocation  // ownerID → {IPv4, IPv6, Version, LastRenew}
```

**GetTunIP 流程：**

```
GetTunIP(ownerID, excludeIPs):
  ├── allocs 中已有且不冲突 → 返回现有 IP（续租 LastRenew）
  ├── allocs 中已有但与 excludeIPs 冲突 →
  │     1. dhcp.RentIPExcluding(excludeIPs) 分配新 IP
  │     2. dhcp.ReleaseIP(旧 IP)
  │     3. 更新 allocs + 持久化
  │     4. 返回新 IP
  └── allocs 中没有 →
        1. dhcp.RentIPExcluding(excludeIPs) 分配新 IP
        2. 存入 allocs + 持久化
        3. 返回新 IP
```

**持久化 (allocs → ConfigMap)：**

分配映射以 JSON 存储在 ConfigMap 的 `TUN_ALLOCS` 键中。

**单用户示例：**
```json
{
  "a1b2c3d4e5f6": {
    "ipv4": "198.18.0.5/16",
    "ipv6": "2001:2::5/64",
    "version": 1717900000000000000,
    "lastRenew": 1717900000
  }
}
```

**多用户 + 多集群示例：**

三个用户同时连接同一集群的 namespace `default`，共享同一个 ConfigMap：
```json
{
  "a1b2c3d4e5f6": {
    "ipv4": "198.18.0.5/16",
    "ipv6": "2001:2::5/64",
    "version": 1717900000000000000,
    "lastRenew": 1717900120
  },
  "f6e5d4c3b2a1": {
    "ipv4": "198.18.0.6/16",
    "ipv6": "2001:2::6/64",
    "version": 1717900100000000000,
    "lastRenew": 1717900200
  },
  "112233445566": {
    "ipv4": "198.18.0.7/16",
    "ipv6": "2001:2::7/64",
    "version": 1717900200000000000,
    "lastRenew": 1717900300
  }
}
```

字段说明：
- `ownerID`（key） — 连接的唯一标识，UUID 前 12 位，每次 connect 生成
- `ipv4` / `ipv6` — 分配的 TUN IP，带 CIDR 掩码
- `version` — 单调递增版本号（`time.Now().UnixNano()`），用于 WatchTunIP 变更检测
- `lastRenew` — 最后续租时间（Unix 秒），LeaseReaper 用此判断是否过期

**过期场景：** 用户 `112233445566` 断连超过 5 分钟后，LeaseReaper 删除该条目并调用 `dhcp.ReleaseIP` 释放 `198.18.0.7`，bitmap 中对应位清零，该 IP 可被新用户复用。

启动时 `loadAllocs` 从 ConfigMap 恢复，过期的分配直接释放。

### 4.3 Lease 机制

**参数：**
- `LeaseDuration = 5 分钟` — IP 分配的有效期
- `LeaseReaper 周期 = 30 秒` — 检查过期 IP 的频率
- `WatchTunIP 续租 = LeaseDuration / 3` ≈ 100 秒 — stream 隐式续租间隔

**续租方式：**

| 来源 | 续租触发 |
|------|---------|
| `GetTunIP` 调用 | 每次调用刷新 `LastRenew` |
| `WatchTunIP` stream | 后台 ticker 每 ~100s 自动续租 |

**过期回收：**

```
LeaseReaper (每 30s):
  for each alloc in allocs:
    if now - alloc.LastRenew > 5min:
      delete(allocs, ownerID)
      dhcp.ReleaseIP(alloc.IPv4, alloc.IPv6)
      saveAllocs()
```

**设计意图：** 无需显式释放 IP。客户端断连后 lease 自然过期，LeaseReaper 回收。避免了断连时清理不完整导致 IP 泄漏。

## 5. ExcludeIPs 冲突避免

### 问题

TUN IP 来自 `198.18.0.0/16`，可能与本地网络接口的 IP 冲突。bitmap 分配器无感知本地网络状况。

### 方案

客户端在 `rentIP` 时采集所有本地接口 IP，作为 `ExcludeIPs` 传给 `GetTunIP`。服务端在 DHCP 分配时跳过这些 IP。

```
Root Daemon (startTUN → rentIP):
  1. collectLocalIPs() → ["192.168.1.100", "198.18.0.1", ...]
  2. GetTunIP(ownerID, excludeIPs=collectLocalIPs())
  3. Server: dhcp.RentIPExcluding(excludeIPs)
  4. 如果分配的 IP 恰好冲突（极小概率竞态）→ isLocalIPConflict → 重试最多 15 次
```

**为什么不用先释放再分配？** bitmap 使用 `contiguousScanStrategy`，从 offset 0 顺序扫描。释放后重新分配总是拿回同一个 IP，导致死循环。`ExcludeIPs` 在单次 DHCP 事务中跳过冲突 IP，一次解决。

## 6. 数据流（完整 IP 分配路径）

```
kubevpn connect -n default
  │
  ├─ User Daemon:
  │   CreateOutboundPod → 确保 traffic manager pod 运行
  │   → req.OwnerID = uuid[:12]
  │   → cli.Connect(req) → Root Daemon
  │
  ├─ Root Daemon:
  │   DoConnect → NetworkManager.Start()
  │     → portForward(:10801, :10802, :9002)
  │     → startTUN:
  │         1. collectLocalIPs()
  │         2. gRPC GetTunIP(ownerID, excludeIPs) → TunConfigServer
  │         3. TunConfigServer:
  │            ├─ dhcp.RentIPExcluding(excludeIPs) → bitmap 分配
  │            ├─ 存入 allocs[ownerID]
  │            └─ 返回 198.18.0.5/16
  │         4. isLocalIPConflict? → 重试 or 使用
  │         5. tun.Listener(tunConfig{Addr: "198.18.0.5/32"})
  │     → StartIPWatcher → WatchTunIP(ownerID)
  │
  ├─ IP 变更：
  │   TunConfigServer → ReconcileDHCP / 外部触发
  │     → NotifyIPChange → WatchTunIP stream push
  │     → Root Daemon: ChangeTunIP(newIPv4, newIPv6)
  │
  └─ 断连：
      → Lease 到期 (5 min) → LeaseReaper 回收
      → bitmap 中对应位清零 → IP 可被新用户复用
```

## 7. ConfigMap 数据格式

ConfigMap 名称：`kubevpn-traffic-manager`

| 键 | 格式 | 内容 |
|---|------|------|
| `DHCP` | Base64 编码的 bitmap | IPv4 分配状态位图 |
| `DHCP6` | Base64 编码的 bitmap | IPv6 分配状态位图 |
| `TUN_ALLOCS` | JSON | `map[ownerID]{ipv4, ipv6, version, lastRenew}` |
| `ENVOY_CONFIG` | YAML | envoy 路由规则 `[]*Virtual` |
| `IPv4_POOLS` | 文本 | 集群 IPv4 地址池 |

bitmap 和 allocs 通过 K8s 的 `resourceVersion` 乐观锁保证一致性。

## 8. 故障场景

| 场景 | 行为 |
|------|------|
| 客户端异常断连 | Lease 5 分钟后过期，LeaseReaper 自动回收 |
| Traffic Manager Pod 重启 | `loadAllocs` 从 ConfigMap 恢复内存状态，bitmap 不丢失 |
| ConfigMap 被手动删除 | `InitDHCP` 重新创建空 ConfigMap，所有 IP 重新分配 |
| 两个用户同时分配 | `RetryOnConflict` 乐观锁保证原子性，后者重试 |
| 分配的 IP 与本地接口冲突 | `ExcludeIPs` 机制跳过冲突 IP |
| IP 池耗尽 | `AllocateNext` 返回错误，连接失败 |
| WatchTunIP 长连接 | 每 ~100s 隐式续租，防止 LeaseReaper 误回收 |

## 9. 关联文件

| 文件 | 作用 |
|------|------|
| `pkg/dhcp/dhcp.go` | DHCP Manager — bitmap 分配器封装 |
| `pkg/controlplane/tun_config.go` | TunConfigServer — IP 分配控制面 |
| `pkg/config/config.go` | CIDR、ConfigMap 键名等常量 |
| `pkg/handler/network.go` | NetworkManager.rentIP — 客户端 IP 获取 |
| `pkg/daemon/rpc/daemon.proto` | TunIPRequest/TunIPResponse proto 定义 |
