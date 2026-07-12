# Unified Proxy Mode and TUN IP Hot-Update Design

## 1. 问题描述

### 1.1 场景

用户执行 `kubevpn proxy deploy/web` 后电脑休眠超过 5 分钟，DHCP 租约过期，TUN IP 被回收。电脑恢复后客户端重新获取了新的 TUN IP，但集群侧的路由规则（envoy xDS 配置 / VPN sidecar iptables DNAT）仍指向旧 IP，导致流量中断。

### 1.2 详细时间线

```
T0: kubevpn proxy deploy/web --headers version=v1
    ┌─ User Daemon ──────────────────────────────────────────────────────┐
    │ getSudoTunIPs() → tunV4 = "198.18.0.5"                            │
    │ CreateRemoteInboundPod(tunV4="198.18.0.5", ownerID="abc123")       │
    │   → addEnvoyConfig: Rule{LocalTunIPv4:"198.18.0.5", OwnerID:"abc"}│
    │   → AddVPNContainer: env LocalTunIPv4=198.18.0.5                   │
    │     → sidecar 启动脚本: iptables DNAT --to 198.18.0.5              │
    └────────────────────────────────────────────────────────────────────┘
    ┌─ Root Daemon ──────────────────────────────────────────────────────┐
    │ Start() → portForward() → startTUN() → rentIP()                    │
    │   → GetTunIP(ownerID="abc123") → 分配 198.18.0.5                   │
    │   → TUN 设备创建: 198.18.0.5                                       │
    │ StartIPWatcher() → WatchTunIP(ownerID="abc123") stream 建立         │
    │ heartbeats() → ICMP src=198.18.0.5 → RouteHub 注册 198.18.0.5      │
    └────────────────────────────────────────────────────────────────────┘
    ┌─ Traffic Manager ──────────────────────────────────────────────────┐
    │ allocs["abc123"] = {IPv4: 198.18.0.5, LastRenew: T0}               │
    │ ConfigMap ENVOY_CONFIG: Rule{LocalTunIPv4:"198.18.0.5"}            │
    │ Processor → xDS snapshot → envoy 路由: version=v1 → 198.18.0.5    │
    └────────────────────────────────────────────────────────────────────┘
    ✅ 流量正常

T1: 电脑休眠（合盖/锁屏）
    本地进程全部挂起: heartbeats 停止, port-forward 断开, WatchTunIP 断开

T1 + 5min: LeaseReaper 回收 IP
    reapExpiredLeases → delete(allocs, "abc123") → dhcp.ReleaseIP(198.18.0.5)
    ⚠️ ConfigMap ENVOY_CONFIG 中 Rule.LocalTunIPv4 仍是 "198.18.0.5"

T2: 电脑恢复
    portForward 重连 → 启动新 healthCheckPortForward(domain="kubevpn-traffic-manager.ns")
    healthCheckPortForward 30s 后发送 DNS 查询（经 gudp relay）
    → 验证 port-forward + gudp + DNS 容器是否存活
    → 不调用 GetTunIP，不触发 IP 重分配

    IPWatcher.doWatchTunIP 重连 WatchTunIP stream
    → WatchTunIP 只在 NotifyIPChange 时推送
    → ⚠️ GetTunIP 新分配不调用 NotifyIPChange → stream 永远收不到推送

T2 + 30s: 流量断裂
    本地 TUN:      198.18.0.5 (旧)
    服务端 allocs:  198.18.0.7 (新, healthCheck 触发的重分配)
    ENVOY_CONFIG:  198.18.0.5 (旧, 从未更新)
    sidecar DNAT:  198.18.0.5 (旧, 启动时硬编码)

    ⚠️ 如果另一用户此时拿到 198.18.0.5 → 路由冲突 → 流量互串
```

### 1.3 影响范围

| 模式 | 受影响 | 断裂点 |
|------|--------|--------|
| **VPN-only** | ❌ 是 | sidecar `iptables DNAT --to ${旧IP}` 硬编码，运行期不更新；服务端 alloc IP 与本地 TUN IP 不一致 |
| **Mesh** | ❌ 是 | ConfigMap `ENVOY_CONFIG` 中 `Rule.LocalTunIPv4` 为旧 IP；envoy xDS 路由到旧 IP |
| **Fargate** | ✅ 否 | `LocalTunIPv4` 固定 `127.0.0.1`，不依赖 TUN IP |

### 1.4 根因分析

#### 问题 1（已修复）：旧 healthCheckTCPConn 的 GetTunIP 触发静默重分配

**已修复**：健康检查已从 gRPC `GetTunIP` 改为 DNS 查询（`healthCheckPortForward`），通过 gudp relay 发送 DNS 请求验证数据面存活。不再调用 `GetTunIP`，彻底消除了健康检查触发静默 IP 重分配的问题。

#### 问题 2：GetTunIP 新分配 IP 时不触发 WatchTunIP 推送

**位置**：`pkg/controlplane/tun_config.go:227-250`（新分配路径）

```go
s.allocs[req.OwnerID] = alloc
go s.saveAllocs(context.Background())
return &rpc.TunIPResponse{...}, nil  // ← 没有调用 NotifyIPChange
```

`ReconcileDHCP` 中 IP 变化时**会调用** `NotifyIPChange`，但 `GetTunIP` 的新分配和重分配路径都没有。`WatchTunIP` stream 收不到推送，`ChangeTunIP` 永远不被调用，本地 TUN IP 不一致**无法自愈**。

#### 问题 3：ConfigMap ENVOY_CONFIG 和 sidecar iptables 不感知 IP 变化

即使问题 1 和 2 被修复（本地 TUN IP 正确更新），两个下游组件仍不感知变化：

| 组件 | 问题 | 位置 |
|------|------|------|
| ConfigMap `ENVOY_CONFIG` 中 `Rule.LocalTunIPv4` | inject 时写入后从未更新，envoy xDS 路由到旧 IP | `inject/envoy.go` addEnvoyConfig 仅在 inject 时调用 |
| VPN sidecar iptables DNAT | 容器启动脚本硬编码 `DNAT --to ${LocalTunIPv4}`，运行期不更新 | `inject/container.go:191` |

**多用户安全风险**：用户 A 恢复后流量仍走旧 IP 198.18.0.5，但服务端认为 A 的 IP 是 198.18.0.7。如果用户 B 此时拿到 198.18.0.5，RouteHub 条目冲突，**流量互串**。

---

## 2. ENVOY_CONFIG 配置说明

### 2.1 存储位置

路由配置存储在 ConfigMap `kubevpn-traffic-manager` 的 `ENVOY_CONFIG` key 中（`config.KeyEnvoy`），格式为 YAML 序列化的 `[]*Virtual` 数组。

**改造前**（当前代码）：

```yaml
# ConfigMap: kubevpn-traffic-manager（改造前）
DHCP: "base64..."          # IPv4 bitmap 单独一个 key（命名不准确，非真正 DHCP 协议）
DHCP6: "base64..."         # IPv6 bitmap 单独一个 key（冗余拆分）
IPv4_POOLS: "10.96.0.0/12 10.244.0.0/16 fd00::/56"  # 名字叫 IPv4 但实际含 IPv6（命名不准确）
TUN_ALLOCS: |              # ...
ENVOY_CONFIG: |            # ...
```

**改造后**：`DHCP`+`DHCP6` 合并为 `TUN_IP_POOL`，`IPv4_POOLS` 改名为 `CLUSTER_CIDRS`：

```yaml
# ConfigMap: kubevpn-traffic-manager（改造后）

TUN_IP_POOL: |                         # TUN IP 地址池（合并原 DHCP + DHCP6）
  ipv4:                                #   bitmap 通过 cilium/ipam ContiguousAllocationMap 管理
    cidr: 198.18.0.0/16                #   RentIP/ReleaseIP 同时操作 v4+v6
    bitmap: "base64-encoded..."        #   65534 个可用地址
  ipv6:
    cidr: 2001:2::/64
    bitmap: "base64-encoded..."

TUN_ALLOCS: |                          # TUN IP 分配映射 (ownerID → 双栈 IP)
  a1b2c3d4e5f6:                        #   ownerID = UUID[:12] (客户端) 或 podName (sidecar)
    ipv4: 198.18.0.5/16
    ipv6: 2001:2::5/64
    version: 1717900000000000000
    lastRenew: 1717900120
  f6e5d4c3b2a1:
    ipv4: 198.18.0.6/16
    ipv6: 2001:2::6/64
    version: 1717900100000000000
    lastRenew: 1717900200

CLUSTER_CIDRS: "10.96.0.0/12 10.244.0.0/16 fd00:10:244::/56"
                                       # 集群 CIDR 缓存（改名，原 IPv4_POOLS）
                                       #   空格分隔的 CIDR 字符串（encodeCIDRs/parseCachedCIDRs）
                                       #   含 Service CIDR + Pod CIDR (IPv4 + IPv6)
                                       #   探测来源: kube-controller-manager / CNI IPAM / Node PodCIDR
                                       #   用于配置本地路由表（哪些 CIDR 走 TUN 隧道）

ENVOY_CONFIG: |                        # envoy 路由配置 ← 本文档核心
  - schemaVersion: 2
    Uid: deployments.apps.web
    ...
```

**ConfigMap 数据结构改动**：

| 改造项 | 改造前 | 改造后 | 涉及文件 |
|--------|--------|--------|---------|
| TUN IP 池 | `DHCP`（v4）+ `DHCP6`（v6）两个 key，各存 base64 | 合并为 `TUN_IP_POOL` 一个 key，YAML 结构体含 `ipv4.bitmap` + `ipv6.bitmap` | `pkg/config/config.go`、`pkg/dhcp/dhcp.go`、`pkg/handler/traffmgr.go` |
| 集群 CIDR | `IPv4_POOLS`（命名不准确，实际含 v6） | `CLUSTER_CIDRS`，空格分隔 CIDR 字符串 | `pkg/config/config.go`、`pkg/handler/connect.go`、`pkg/handler/traffmgr.go`、`pkg/dhcp/dhcp.go` |

### 2.2 数据模型

```
ENVOY_CONFIG ([]*Virtual)
│
├── Virtual[0]                          ← 一个被代理的 workload
│   ├── SchemaVersion: 2                ← 配置版本（当前 CurrentSchemaVersion=2，要求 OwnerID）
│   ├── UID: "deployments.apps.web"     ← workload 标识 (group.resource.name)
│   ├── Namespace: "default"            ← workload 命名空间
│   ├── FargateMode: false              ← 是否 Fargate/Service 模式
│   ├── Ports:                          ← workload 暴露的端口
│   │   └── [{ContainerPort: 8080, Protocol: TCP, EnvoyListenerPort: 0}]
│   └── Rules:                          ← 路由规则（每个 proxy 用户一条）
│       ├── Rule[0]                     ← 用户 A 的规则
│       │   ├── Headers: {version: v1}  ← header 匹配条件（空=全量劫持）
│       │   ├── LocalTunIPv4: "198.18.0.5"  ← 用户 A 的 TUN IP（envoy 路由目标）
│       │   ├── LocalTunIPv6: "2001:2::5"
│       │   ├── OwnerID: "a1b2c3d4e5f6"    ← 用户 A 的连接 UUID[:12]
│       │   └── PortMap: {8080: "9080"}     ← 端口映射
│       └── Rule[1]                     ← 用户 B 的规则
│           ├── Headers: {version: v2}
│           ├── LocalTunIPv4: "198.18.0.9"
│           ├── OwnerID: "f6e5d4c3b2a1"
│           └── PortMap: {8080: "9080"}
│
├── Virtual[1]                          ← 另一个被代理的 workload
│   └── ...
```

**YAML 示例**：

```yaml
- schemaVersion: 2
  Uid: deployments.apps.web
  namespace: default
  ports:
  - containerPort: 8080
    protocol: TCP
  rules:
  - headers:
      version: v1
    localtunipv4: "198.18.0.5"
    localtunipv6: "2001:2::5"
    ownerID: "a1b2c3d4e5f6"
    portmap:
      8080: "9080"
  - headers:
      version: v2
    localtunipv4: "198.18.0.9"
    localtunipv6: "2001:2::9"
    ownerID: "f6e5d4c3b2a1"
    portmap:
      8080: "9080"
```

### 2.3 关键字段说明

| 字段 | 说明 |
|------|------|
| `SchemaVersion` | 配置版本。0=旧版（无 OwnerID）；2=当前版本（OwnerID 必填）|
| `UID` | workload 标识，格式 `group.resource.name`，如 `deployments.apps.web`。用 `util.GenEnvoyUID(namespace, uid)` 生成 xDS 的 nodeID |
| `FargateMode` | `true` 时 envoy 用 `BindToPort=true`（直接绑定端口），Service targetPort 被修改；`false` 时用 `use_original_dst`（iptables 重定向）|
| `Ports[].ContainerPort` | 原始容器端口 |
| `Ports[].EnvoyListenerPort` | Fargate 模式下 envoy 绑定的随机端口（非 Fargate 时为 0）|
| `Rules[].Headers` | envoy header 路由匹配条件。**为空时匹配所有请求**（VPN-only 全量劫持）|
| `Rules[].LocalTunIPv4/v6` | **envoy 路由目标 IP** — 用户本地 TUN 设备的 IP。**本文档修复的核心字段**：IP 变化时需要自动更新 |
| `Rules[].OwnerID` | 连接唯一标识（UUID[:12]），用于规则匹配和删除。inject 时生成，Rule 的主键 |
| `Rules[].PortMap` | 端口映射。格式 `containerPort → "envoyPort"` 或 `containerPort → "envoyPort:localPort"`（Fargate 模式）|

### 2.4 Rule 写入/更新/删除

**写入** — `addEnvoyConfig(ctx, configMapInterface, envoyRuleSpec)`（`pkg/inject/envoy.go`）：

通过 `addVirtualRule` 处理四种场景：

| Case | 条件 | 动作 |
|------|------|------|
| 1 | 新 workload（无匹配的 Virtual） | 创建 Virtual + Rule |
| 2 | 同一用户更新（OwnerID 匹配） | 更新 LocalTunIPv4/v6、合并 Headers/PortMap |
| 3 | 不同用户相同 headers（header 匹配） | 替换 OwnerID 和 IP（header 所有权转移）|
| 4 | 新用户不同 headers | 追加 Rule |

**本方案新增**：`syncEnvoyRuleIP` 利用 **Case 2** 的逻辑——OwnerID 匹配时自动更新 `LocalTunIPv4/v6`，无需额外代码。

**删除** — `removeEnvoyConfig(ctx, configMapInterface, namespace, nodeID, ownerID)`：

按 OwnerID 删除匹配的 Rule。如果 Virtual 的最后一条 Rule 被删除，整个 Virtual 条目移除。

### 2.5 ENVOY_CONFIG → xDS 推送链路

```
addEnvoyConfig / syncEnvoyRuleIP
  → 更新 ConfigMap ENVOY_CONFIG
  → Watcher (K8s informer, pkg/controlplane/watcher.go)
    → 检测 ConfigMap 变化
    → notifyCh <- NotifyMessage{Content: configMap.Data["ENVOY_CONFIG"]}
  → Processor (pkg/controlplane/processor.go)
    → parseYaml(content) → []*Virtual
    → 对每个 Virtual:
      → nodeID = GenEnvoyUID(namespace, uid)
      → 检查 expireCache（5min TTL，跳过未变化的配置）
      → Virtual.To(enableIPv6) → 生成 xDS 资源:
        ├── Listener (TCP: use_original_dst / Fargate: BindToPort / UDP: udp_proxy)
        ├── Route    (header 匹配 → 特定 cluster / 默认 → origin_cluster)
        ├── Cluster  ({tunIP}_{envoyPort})
        └── Endpoint ({tunIP}:{envoyPort} — LocalTunIPv4 在这里被消费)
      → cache.SetSnapshot(nodeID, snapshot)
  → envoy sidecar
    → ADS gRPC stream (连接到 :9002 xDS server)
    → 收到新 snapshot → 路由热更新
```

**关键**：`LocalTunIPv4` 最终在 `toEndPoint` 中转化为 envoy endpoint 的 address。IP 变化 → ENVOY_CONFIG 更新 → Processor 生成新 endpoint → xDS 推送 → envoy 路由到新 IP。这就是本方案利用的现有链路。

### 2.6 多用户场景

```yaml
# 用户 A (version=v1) 和用户 B (version=v2) 同时代理 deploy/web
- schemaVersion: 2
  Uid: deployments.apps.web
  namespace: default
  ports:
  - containerPort: 9080
    protocol: TCP
  rules:
  - headers: {x-user: alice}
    localtunipv4: "198.18.0.5"
    ownerID: "a1b2c3d4e5f6"
    portmap: {9080: "9080"}
  - headers: {x-user: bob}
    localtunipv4: "198.18.0.6"
    ownerID: "f6e5d4c3b2a1"
    portmap: {9080: "9080"}
```

envoy 路由：
- `x-user: alice` → `198.18.0.5:9080`（Alice 本地）
- `x-user: bob` → `198.18.0.6:9080`（Bob 本地）
- 不匹配 → `origin_cluster`（ORIGINAL_DST，回原服务）

Alice 休眠恢复后 IP 变为 `198.18.0.7`：`syncEnvoyRuleIP` 更新 `Rule[0].LocalTunIPv4 = "198.18.0.7"` → xDS 推送 → envoy 自动路由到新 IP。Bob 不受影响。

---

## 3. 设计目标

1. IP 变化时**自动更新** ENVOY_CONFIG（服务端主动推送，非客户端轮询）
2. 客户端（Root Daemon）通过修复后的 **WatchTunIP 推送链路**感知 IP 变化，自动更新本地 TUN 设备
3. **统一 VPN-only 和 Mesh 注入策略**：消除 `vpnInjector`，VPN-only = headers 为空的 Mesh，全部通过 envoy 路由（TCP + UDP）
4. 所有模式的路由配置变更通过 **xDS 推送自动热更新**，与 envoy 完全对齐
5. **双保险**：`NotifyIPChange` 事件推送（实时）+ `WatchTunIP` ticker 定时推送（兜底）

---

## 3. 方案总览

```
                            电脑恢复
                               │
                    Root Daemon 重连
                               │
               healthCheck → GetTunIP(ownerID) → 新 IP 198.18.0.7
                               │
              TunConfigServer 检测到 IP 变化
                               │
         ┌─────────────────────┼───────────────────────────┐
         ▼                     ▼                           ▼
   更新 allocs           更新 ConfigMap              notifyWatchers
   (内存)                ENVOY_CONFIG                → WatchTunIP 推送
                         Rule.LocalTunIPv4                 │
                               │                    Root Daemon:
         ┌─────────────────────┤                    ChangeTunIP
         ▼                     ▼                    本地 TUN 更新 ✅
   Watcher 感知           Processor
   ConfigMap 变化          生成新 xDS 快照    ┌──────────────────┐
         │                     │              │ 定时兜底 (~100s)  │
         ▼                     ▼              │ WatchTunIP ticker │
   envoy sidecar         TCP+UDP 路由         │ 补发当前 IP       │
   ADS 推送               全部热更新 ✅        └──────────────────┘
```

### 三个修复层次

| 层次 | 修复内容 | 解决的问题 |
|------|---------|-----------|
| **Step 0** | `GetTunIP` 新分配 IP 时调用 `notifyWatchers` | 问题 2: WatchTunIP 推送链路断裂 |
| **Step 0.5** | `WatchTunIP` ticker 定时推送当前 IP | 兜底: stream 重连时错过的事件 |
| **Step 1** | `GetTunIP` IP 变化时调用 `syncEnvoyRuleIP` 更新 ConfigMap | 问题 3: ENVOY_CONFIG 不感知变化 |
| **Step 2** | 统一 VPN-only/Mesh → 全走 envoy（TCP+UDP） | 问题 3: 消除 iptables DNAT 硬编码 |

---

## 4. 详细设计

### 4.1 Step 0: GetTunIP 新分配 IP 时调用 notifyWatchers

**位置**：`pkg/controlplane/tun_config.go` — `GetTunIP`

当前 `GetTunIP` 的新分配路径和重分配路径分配了新 IP 后直接返回，没有通知 `WatchTunIP` stream。修复：在分配新 IP 后调用 `notifyWatchers`。

**新分配路径（line 227-250）**：

```go
// 修复前:
s.allocs[req.OwnerID] = alloc
go s.saveAllocs(context.Background())
return &rpc.TunIPResponse{...}, nil

// 修复后:
s.allocs[req.OwnerID] = alloc
s.saveAllocs(ctx)  // 同步，不再 fire-and-forget

// 通知 WatchTunIP stream（IPWatcher 会调用 ChangeTunIP 更新本地 TUN）
s.notifyWatchers(req.OwnerID, &rpc.TunIPResponse{
    IPv4: alloc.IPv4.String(), IPv6: alloc.IPv6.String(), Version: alloc.Version,
})

// 同步更新 ENVOY_CONFIG（见 Step 1）
go s.syncEnvoyRuleIP(context.Background(), req.OwnerID, alloc.IPv4, alloc.IPv6)

return &rpc.TunIPResponse{...}, nil
```

**重分配路径（line 188-224）**：同样在 `s.allocs[req.OwnerID] = newAlloc` 后添加 `notifyWatchers` + `syncEnvoyRuleIP`。

**`notifyWatchers` 提取为独立方法**（原来的推送逻辑散布在 `NotifyIPChange` 中）：

```go
func (s *TunConfigServer) notifyWatchers(ownerID string, resp *rpc.TunIPResponse) {
    for _, ch := range s.watchers[ownerID] {
        select {
        case ch <- resp:
        default:
        }
    }
}
```

### 4.2 Step 0.5: WatchTunIP 定时推送当前 IP（兜底）

**位置**：`pkg/controlplane/tun_config.go` — `WatchTunIP`

当前 `WatchTunIP` 的 ticker 每 `LeaseDuration/3`（~100s）只做续约。扩展为续约 + 推送当前 IP：

```go
case <-ticker.C:
    s.mu.Lock()
    s.renewLease(req.OwnerID)
    // 兜底：定时推送当前 IP
    if alloc, ok := s.allocs[req.OwnerID]; ok {
        resp := &rpc.TunIPResponse{Version: alloc.Version}
        if alloc.IPv4 != nil {
            resp.IPv4 = alloc.IPv4.String()
        }
        if alloc.IPv6 != nil {
            resp.IPv6 = alloc.IPv6.String()
        }
        select {
        case ch <- resp:
        default:
        }
    }
    s.mu.Unlock()
```

客户端侧 `doWatchTunIP` 已有 version 对比（`resp.Version != *currentVersion`），IP 未变时不触发 `ChangeTunIP`，零副作用。

**双保险**：

| 机制 | 触发时机 | 延迟 | 覆盖场景 |
|------|---------|------|---------|
| `notifyWatchers` 事件推送 | `GetTunIP` 分配新 IP 时 | 毫秒级 | stream 存活时实时感知 |
| ticker 定时推送 | 每 ~100s | 最大 100s | stream 重连后补发、推送丢失恢复 |

### 4.3 Step 1: GetTunIP IP 变化时服务端自动更新 ENVOY_CONFIG

**位置**：`pkg/controlplane/tun_config.go` — 新增 `syncEnvoyRuleIP`

当 `GetTunIP` 分配了与之前不同的 IP 时，用 ownerID 在 ENVOY_CONFIG 中查找匹配的 Rule，更新 `LocalTunIPv4/v6`。

```go
func (s *TunConfigServer) syncEnvoyRuleIP(ctx context.Context, ownerID string, newIPv4, newIPv6 *net.IPNet) {
    err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
        cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(
            ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
        if err != nil {
            return err
        }
        virtuals, err := parseYaml(cm.Data[config.KeyEnvoy])
        if err != nil {
            return err
        }

        changed := false
        for _, v := range virtuals {
            for _, rule := range v.Rules {
                if rule.OwnerID == ownerID && rule.LocalTunIPv4 != newIPv4.IP.String() {
                    rule.LocalTunIPv4 = newIPv4.IP.String()
                    if newIPv6 != nil {
                        rule.LocalTunIPv6 = newIPv6.IP.String()
                    }
                    changed = true
                }
            }
        }
        if !changed {
            return nil
        }

        data, err := yaml.Marshal(virtuals)
        if err != nil {
            return err
        }
        cm.Data[config.KeyEnvoy] = string(data)
        _, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
        return err
    })
    if err != nil {
        plog.G(ctx).Errorf("[TunConfig] syncEnvoyRuleIP failed for owner %s: %v", ownerID, err)
    }
}
```

ConfigMap 更新后，现有链路自动生效：

```
ConfigMap 变化 → Watcher (informer) → Processor.ProcessFile
→ Virtual.To() → xDS resources → cache.SetSnapshot(nodeID)
→ envoy sidecar ADS 推送 → 路由热更新 ✅
```

**Mesh 模式在此步已完成修复**，零额外改动。

### 4.4 Step 2: 统一 VPN-only 和 Mesh 模式 — 全部走 envoy

#### 4.4.1 设计理念

VPN-only 模式可以视为 **headers 为空的 Mesh 模式**。

`toRoute(clusterName, emptyHeaders)` 生成的 envoy 路由：`RouteMatch{Prefix: "/", Headers: nil}`——匹配所有请求（无 header 约束），效果等同于全量劫持。

envoy 支持 UDP proxy（`envoy.extensions.filters.udp.udp_proxy.v3.UdpProxyConfig`），通过 `cluster → endpoint` 路由 UDP 流量，与 TCP 相同的 xDS 推送模型。因此 TCP + UDP 都可以通过 envoy 转发。

**统一后**：

```
改造前：
  VPN-only:  iptables DNAT → ${LocalTunIPv4}  ← 硬编码，TCP+UDP，不可热更新
  Mesh:      iptables DNAT → :15006 (envoy)    ← 固定端口，TCP only，xDS 热更新

改造后：
  统一 Mesh: iptables DNAT → :15006 (envoy)    ← 固定端口，TCP+UDP，xDS 热更新
             headers 为空 → envoy 匹配一切 → 全量转发（原 VPN-only 行为）
             headers 非空 → envoy header 路由 → 分流（原 Mesh 行为）
```

**结果**：消除 `vpnInjector`，删除 `AddVPNContainer`。所有 proxy 模式统一走 `meshInjector`（VPN + envoy sidecar）。

#### 4.4.2 TCP vs UDP 路由差异

TCP 和 UDP 在 envoy 中的路由机制不同：

**TCP**（现有，不变）：
- `use_original_dst=true`：envoy 用 `SO_ORIGINAL_DST` socket option 恢复 iptables DNAT 前的原始目标
- header 匹配 → 转到用户 IP；不匹配 → `origin_cluster`（`ORIGINAL_DST` cluster）回原服务
- `origin_cluster` 依赖 `SO_ORIGINAL_DST`，仅 TCP 有效

**UDP**（新增）：
- `udp_proxy` filter 通过 `cluster` 直接指定上游，**不支持 header 路由**（UDP 无 header 概念）
- UDP 不支持 `SO_ORIGINAL_DST`，不能用 `origin_cluster`
- UDP listener 使用 `BindToPort=true` 绑定到容器 UDP 端口（类似 Fargate 模式）

**各场景行为**：

| 场景 | TCP 路由 | UDP 路由 |
|------|---------|---------|
| **VPN-only（headers 空）** | envoy route 全匹配 → cluster → 用户 IP ✅ | udp_proxy → 同一 cluster → 用户 IP ✅ |
| **Mesh（有 headers）** | header 匹配 → 用户 IP；不匹配 → `origin_cluster` ✅ | udp_proxy → 全部到用户 IP（UDP 无 header 分流，与现有行为一致）|

> **注**：Mesh 模式下 UDP 无法按 header 分流是 envoy 的固有限制。当前 Mesh 模式的 VPN sidecar（iptables DNAT → :15006）也不支持 UDP header 分流，所以行为不退化。

#### 4.4.3 envoy UDP listener 配置

在 `Virtual.To()` 生成的 xDS listener 中，为每个 UDP 端口生成 UDP listener：

```yaml
- name: "{ns}_{uid}_{port}_UDP"
  address:
    socket_address:
      protocol: UDP
      address: "0.0.0.0"
      port_value: {containerPort}
  listener_filters:
  - name: envoy.filters.udp_listener.udp_proxy
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.filters.udp.udp_proxy.v3.UdpProxyConfig
      stat_prefix: udp_proxy
      cluster: "{tunIP}_{envoyRulePort}"  # 复用 TCP 的 cluster → endpoint
```

UDP listener 使用 `BindToPort=true` 直接绑定容器 UDP 端口，不用 `use_original_dst`。cluster/endpoint 与 TCP 完全共用。

#### 4.4.4 `Virtual.To()` 改造

文件：`pkg/controlplane/cache.go`

在现有 TCP listener 生成逻辑旁，为 UDP 端口生成 UDP listener + udp_proxy filter：

```go
func (a *Virtual) To(enableIPv6 bool, logger *log.Entry) (
    listeners, clusters, routes, endpoints []types.Resource,
) {
    for _, port := range a.Ports {
        // ... existing TCP listener + route + cluster + endpoint ...

        // 新增：UDP listener（复用同一 cluster/endpoint）
        if port.Protocol == corev1.ProtocolUDP || port.Protocol == "" {
            udpListener := toUDPListener(listenerName+"_UDP", port, clusterName)
            listeners = append(listeners, udpListener)
        }
    }
    // ...
}

func toUDPListener(name string, port ContainerPort, clusterName string) *listener.Listener {
    return &listener.Listener{
        Name: name,
        Address: &core.Address{
            Address: &core.Address_SocketAddress{
                SocketAddress: &core.SocketAddress{
                    Protocol:      core.SocketAddress_UDP,
                    Address:       "0.0.0.0",
                    PortSpecifier: &core.SocketAddress_PortValue{PortValue: uint32(port.ContainerPort)},
                },
            },
        },
        ListenerFilters: []*listener.ListenerFilter{{
            Name: "envoy.filters.udp_listener.udp_proxy",
            ConfigType: &listener.ListenerFilter_TypedConfig{
                TypedConfig: mustMarshalAny(&udpproxyv3.UdpProxyConfig{
                    StatPrefix:     "udp_proxy",
                    RouteSpecifier: &udpproxyv3.UdpProxyConfig_Cluster{Cluster: clusterName},
                }),
            },
        }},
    }
}
```

#### 4.4.5 消除 vpnInjector

文件：`pkg/inject/injector.go`

```go
// 修改前：三种策略
func NewInjector(opts InjectOptions) Injector {
    if util.IsK8sService(opts.Object) {
        return &fargateInjector{opts: opts}
    }
    if len(opts.Headers) > 0 || len(opts.PortMaps) > 0 {
        return &meshInjector{opts: opts}
    }
    return &vpnInjector{opts: opts}
}

// 修改后：两种策略
func NewInjector(opts InjectOptions) Injector {
    if util.IsK8sService(opts.Object) {
        return &fargateInjector{opts: opts}
    }
    return &meshInjector{opts: opts}  // headers 为空 = 全量劫持
}
```

**删除** `pkg/inject/vpn.go`（`vpnInjector`）和 `AddVPNContainer` 函数。

#### 4.4.6 iptables 规则统一

文件：`pkg/inject/container.go`

所有模式统一使用 Mesh 的 iptables 规则，不再依赖用户 IP：

```shell
# 统一：DNAT 到 envoy 端口
iptables -t nat -A PREROUTING ! -p icmp ! -s 127.0.0.1 ! -d ${CIDR4} -j DNAT --to :15006
ip6tables -t nat -A PREROUTING ! -p icmp ! -s ::1 ! -d ${CIDR6} -j DNAT --to :15006
```

不再需要 `LocalTunIPv4/LocalTunIPv6` 环境变量。`AddVPNContainer` 删除后，统一使用 `AddVPNAndEnvoyContainer`。

#### 4.4.7 客户端保持 WatchTunIP

客户端（Root Daemon）的 TUN IP 变化通过 `WatchTunIP` 推送（Step 0 修复 + Step 0.5 定时兜底）。xDS 按 nodeID（workload 维度）组织快照，客户端没有对应 nodeID，不走 xDS。

```
sidecar 路由热更新: xDS endpoint 推送（复用 ENVOY_CONFIG 链路）
客户端 TUN IP 热更新: WatchTunIP 推送（修复 + 兜底）
```

---

## 5. 完整数据流

```
电脑休眠 5 分钟 → LeaseReaper 回收 IP 198.18.0.5

电脑恢复:
  Root Daemon → port-forward 重连
    → healthCheckPortForward 发送 DNS 查询（经 gudp relay）
    → 验证数据面存活（不调用 GetTunIP，不触发 IP 重分配）

  TunConfigServer.GetTunIP (服务端，同一调用中):
    1. 分配 198.18.0.7，写入 allocs["abc123"]
    2. notifyWatchers → WatchTunIP stream 推送 {IPv4: 198.18.0.7}
    3. syncEnvoyRuleIP → ConfigMap ENVOY_CONFIG Rule.LocalTunIPv4 = 198.18.0.7

  Root Daemon IPWatcher (WatchTunIP stream 收到推送):
    → ChangeTunIP(198.18.0.7) → 本地 TUN 设备更新 ✅
    → 心跳 ICMP src=198.18.0.7 → RouteHub 注册新 IP ✅

  envoy sidecar (统一 xDS 推送):
    → Watcher 感知 ConfigMap ENVOY_CONFIG 变化
    → Processor.ProcessFile → 新 xDS 快照 (endpoint IP=198.18.0.7)
    → cache.SetSnapshot → envoy ADS 推送
    → TCP 路由热更新 ✅
    → UDP 路由热更新 ✅（udp_proxy 同一 cluster/endpoint）

  客户端兜底 (WatchTunIP ticker ~100s):
    → 即使事件推送丢失，100s 内 ticker 定时推送当前 IP
    → version 对比 → 触发 ChangeTunIP ✅

  延迟:
    事件推送: ~毫秒级
    定时兜底: 最大 ~100s
```

---

## 6. 改动文件清单

| 文件 | 改动类型 | 说明 |
|------|---------|------|
| **ConfigMap 数据结构重构** | | |
| `pkg/config/config.go` | 修改 | `KeyDHCP`+`KeyDHCP6` → `KeyTunIPPool`；`KeyClusterIPv4POOLS` → `KeyClusterCIDRs` |
| `pkg/dhcp/dhcp.go` | 修改 | `updateDHCPConfigMap` 改为读写 `TUN_IP_POOL` key（YAML 结构体含 `ipv4.bitmap` + `ipv6.bitmap`） |
| `pkg/handler/connect.go` | 修改 | `getCIDR` 中 `KeyClusterIPv4POOLS` → `KeyClusterCIDRs`；`encodeCIDRs`/`parseCachedCIDRs` 使用空格分隔 CIDR 字符串 |
| `pkg/handler/traffmgr.go` | 修改 | `createOutboundPod` ConfigMap 初始化 key 对齐 |
| **IP 变化推送修复** | | |
| `pkg/controlplane/tun_config.go` | 修改 | `GetTunIP` 新分配时调 `notifyWatchers` + `syncEnvoyRuleIP`；新增 `syncEnvoyRuleIP`；`WatchTunIP` ticker 定时推送 |
| **Proxy 模式统一** | | |
| `pkg/controlplane/cache.go` | 修改 | `Virtual.To()` 为 UDP 端口生成 UDP listener + `udp_proxy` filter；新增 `toUDPListener` |
| `pkg/inject/injector.go` | 修改 | `NewInjector` 去掉 `vpnInjector` 分支，VPN-only 走 `meshInjector` |
| `pkg/inject/vpn.go` | **删除** | `vpnInjector` 不再需要 |
| `pkg/inject/container.go` | 修改 | 删除 `AddVPNContainer`（统一用 `AddVPNAndEnvoyContainer`）；去掉 `LocalTunIPv4/v6` 环境变量 |
| `pkg/inject/envoy.yaml` | 修改 | bootstrap 模板增加 UDP listener 基础配置 |
| `pkg/core/protocol_registry.go` | 简化 | 删除 `watchTunIPChanges` + `pollTunIP` + `applyTunIPChange`（envoy 负责路由） |

**零新增 proto/RPC** — 完全复用现有 ENVOY_CONFIG → Watcher → Processor → xDS 推送链路和 WatchTunIP 推送。

---

## 7. 设计对比

| 维度 | 改造前 | 改造后 |
|------|-------|-------|
| 注入策略 | 3 种（vpn / mesh / fargate） | 2 种（mesh / fargate），VPN-only = headers 为空的 mesh |
| sidecar 容器 | VPN-only: VPN only / Mesh: VPN + envoy | 统一: VPN + envoy |
| sidecar DNAT | VPN-only: `DNAT → ${IP}`（硬编码）/ Mesh: `DNAT → :15006` | 统一: `DNAT → :15006`（固定端口） |
| TCP 路由 | VPN-only: iptables 直接转发 / Mesh: envoy | 统一: envoy（header 路由 + origin_cluster 默认回原服务） |
| UDP 路由 | VPN-only: iptables 转发 / Mesh: 不支持 | 统一: envoy udp_proxy（cluster → endpoint） |
| IP 热更新 | VPN-only: ❌ / Mesh: xDS 推送 | 统一: xDS 推送 |
| ENVOY_CONFIG 更新 | 仅 inject 时写入一次 | `syncEnvoyRuleIP` 在 IP 变化时自动更新 |
| 客户端 TUN IP | WatchTunIP（推送链路断裂） | WatchTunIP（修复 NotifyIPChange + ticker 兜底） |
| 代码量 | vpn.go + AddVPNContainer + watchTunIPChanges | 全部删除，统一走 mesh 路径 |
| 新增服务端代码 | — | `syncEnvoyRuleIP` + `toUDPListener` |
| 额外资源 | — | VPN-only 多一个 envoy 容器（~200MB） |

---

## 8. 兼容性

全新设计，不兼容旧版本 sidecar。升级后需要 `kubevpn reset` 清理旧 sidecar + 重新 `kubevpn proxy` 注入新版 sidecar。

---

## 9. 验证

```bash
# 编译
go build ./...
go vet ./pkg/...

# 单元测试
go test ./pkg/controlplane/... -v
go test ./pkg/inject/... -v

# 集成测试场景:

# 1. VPN-only（无 headers）
kubevpn proxy deploy/web
# → 验证: envoy sidecar 注入、TCP+UDP 全量劫持、流量正常

# 2. Mesh（有 headers）
kubevpn proxy deploy/web --headers version=v1
# → 验证: header 路由正常、不匹配的请求回原服务

# 3. 休眠恢复模拟
# → 在 traffic manager pod 中删除 TUN_ALLOCS 条目（模拟 lease 过期）
# → 等待 healthCheck 触发 GetTunIP（30s 内）
# → 验证:
#    本地 TUN IP 已更新（ip addr show utun0）
#    ConfigMap ENVOY_CONFIG 中 Rule.LocalTunIPv4 已更新
#    envoy 路由已热更新（流量恢复）

# 4. UDP 测试
# → 向被代理 workload 的 UDP 端口发数据
# → 验证 envoy udp_proxy 正确转发到用户本地
```
