# Handler Package Architecture

## Overview

`pkg/handler` 是 VPN 连接、代理管理、文件同步的核心业务逻辑层。职责分布在四个子管理器中：

```
ConnectOptions (会话编排器)
├── NetworkManager      — 完整网络生命周期 (port-forward, TUN, 路由, DNS) + IP 热更新
├── ProxyManager        — Sidecar 注入/卸载
├── ConfigMapStore      — ConfigMap 读写 + 健康检查
└── K8sClient           — Kubernetes 访问（嵌入）
```

## ConnectOptions — 会话编排器

```go
type ConnectOptions struct {
    K8sClient                       // 嵌入: clientset, config, factory

    // 配置 (daemon 层创建时设置)
    ManagerNamespace     string     // traffic manager 所在 namespace
    WorkloadNamespace    string     // 用户工作负载 namespace
    ExtraRouteInfo       ExtraRouteInfo
    Image                string
    OriginKubeconfigPath string
    OwnerID              string     // UUID, 唯一标识本次连接 (用于 TunConfigService)
    RequestRaw           []byte     // 序列化的 ConnectRequest (用于 daemon 重启回放)

    // 会话生命周期
    ctx, cancel          context.Context, context.CancelFunc
    LocalTunIPv4         *net.IPNet // 运行时临时变量 (不持久化)
    LocalTunIPv6         *net.IPNet
    rollbackFuncList     []func() error

    // 子管理器
    network              *NetworkManager
    proxyManager         *ProxyManager
    configMapStore       *ConfigMapStore
}
```

### Dual-Daemon 角色

| | User Daemon (控制面) | Root Daemon (数据面) |
|---|---|---|
| 入口 | `redirectConnectToSudoDaemon` | `Connect` → `DoConnect` |
| IP 获取 | `RentIP` → TunConfigService | `GetIPFromContext` (从 metadata) |
| 子管理器 | proxyManager, configMapStore | network, configMapStore |

### IP 分配流程 (新模型 — TunConfigService)

```
User Daemon:
  RentIP(ctx, "", "")
    → getTunIPFromControlPlane(ctx)
      → port-forward :9002
      → gRPC GetTunIP(ownerID)
      → TunConfigServer DHCP rent
    ← 198.18.0.5/32
  → metadata{IPv4, IPv6, OwnerID} → Root Daemon

Root Daemon:
  GetIPFromContext → 读取 IP + OwnerID
  DoConnect → NetworkManager.Start()
  → NetworkManager.StartIPWatcher(ctx)
    → WatchTunIP(ownerID) stream
    → IP 变化 → ChangeTunIP()
```

**不再有 client 侧 DHCP。** IP 完全由 TunConfigService (server 端) 管理。

## NetworkManager — 完整网络生命周期

```go
type NetworkManager struct {
    cfg       NetworkConfig    // 不可变配置
    ctx       context.Context  // 独立上下文 (Stop 只取消网络)
    cancel    context.CancelFunc
    tunName   string
    extraHost []dns.Entry
    dnsConfig *dns.Config
}

type NetworkConfig struct {
    Clientset, RESTClient, Config  // K8s 访问
    ManagerNamespace, WorkloadNamespace
    LocalTunIPv4, LocalTunIPv6     // TUN 设备 IP
    CIDRs, APIServerIPs            // 路由参数
    ExtraRouteInfo                 // 额外路由/域名
    Image, Lock                    // 辅助
    OwnerID                        // 用于 IP watcher
    GetRunningPodList              // 依赖注入
}
```

### 生命周期 API

| 方法 | 说明 |
|------|------|
| `Start(ctx)` | 启动: port-forward → TUN → 路由 → DNS |
| `Stop()` | 停止: DNS → 取消上下文 |
| `ChangeTunIP(ctx, v4, v6)` | 热更新 TUN IP (不重建网络栈) |
| `StartIPWatcher(ctx)` | 后台监听 TunConfigService 推送 IP 变更 |
| `TunName()` | 获取 TUN 设备名 |

### IP 热更新流程

```
TunConfigService push (WatchTunIP stream)
  → NetworkManager.ChangeTunIP(newIPv4, newIPv6)
    → tun.ChangeIP(tunName, old, new)     // OS 层修改 (fd 不变)
    → 更新 cfg.LocalTunIPv4/v6
    → heartbeat 下次 tick 自动使用新 IP   // 从 OS 重读
    → Server RouteHub 自动注册新 IP       // 每包 AddRoute
```

## TunConfigService — IP 配置中心

运行在 Traffic Manager Pod 的 control-plane gRPC server (端口 9002) 上：

```protobuf
service TunConfigService {
  rpc GetTunIP(TunIPRequest) returns (TunIPResponse);      // 分配/续租
  rpc WatchTunIP(TunIPRequest) returns (stream TunIPResponse); // 变更推送
  // 无 ReleaseTunIP — 遵循 DHCP 协议，lease 过期自动回收
}

message TunIPRequest {
  string OwnerID = 1;
  string Namespace = 2;
  repeated string ExcludeIPs = 4;  // client 本地接口 IP，分配时跳过
}
```

### 冲突避免 — ExcludeIPs

- `rentIP` 将本地所有接口 IP 作为 `ExcludeIPs` 传给 server
- server 调用 `dhcp.RentIPExcluding(excludeIPs)`，在单次 DHCP 事务中跳过冲突 IP
- 通常一次调用即可；保留轻量重试（最多 15 次）处理非原子竞态

### 续租机制

- `GetTunIP` 每次调用刷新 `LastRenew` 时间戳（兼做续租）
- `WatchTunIP` stream 活跃时，server 端每 `LeaseDuration/3`（约 100s）自动刷新 `LastRenew`（隐式续租）
- `StartLeaseReaper` 后台每 30s 扫描过期分配 (TTL = 5 min)
- 过期 IP 自动回收到 DHCP 池
- **不再需要显式 Release** — crash 后 lease 自动过期回收
- WatchTunIP stream 断开后，如果 client 不再调用 GetTunIP，IP 5 分钟后回收
- 详见 [23-watchtunip-lease-renewal-fix.md](23-watchtunip-lease-renewal-fix.md)

### OwnerID

| 场景 | OwnerID 值 | 生命周期 |
|------|-----------|---------|
| Client (connect/proxy) | `uuid.New().String()[:12]` | 每次连接唯一 |
| Sidecar (mesh mode) | `podName` | Pod 生命周期 |

**为什么不用 connectionID：** connectionID = namespace UID 后 12 位，是 per-namespace 的，多个 client 连同一 namespace 时冲突。

## ProxyManager — Sidecar 注入/卸载

```go
type ProxyManager struct {
    factory            cmdutil.Factory
    clientset          kubernetes.Interface
    managerNamespace   string
    mu                 sync.Mutex
    workloads          ProxyList
}
```

| 方法 | 说明 |
|------|------|
| `Add(proxy)` | 注册代理工作负载 |
| `Remove(ns, workload)` | 移除跟踪 |
| `Resources()` | 快照列表 |
| `IsMe(ns, uid, headers)` | 所有权检查 |
| `LeaveAll(ctx, v4)` | 卸载所有 sidecar |
| `Leave(ctx, resources, v4)` | 卸载指定工作负载 |

## ConfigMapStore — ConfigMap + 健康检查

```go
type ConfigMapStore struct {
    clientset, managerNamespace
    informerOnce, informer, informerStop
    healthStatus HealthStatus
}
```

**延迟初始化：** 通过 `getConfigMapStore()` 首次访问时创建，确保 `ManagerNamespace` 已被 `detectAndSetManagerNamespace` 修正。

## Namespace 规范

| 字段 | 含义 |
|------|------|
| `ManagerNamespace` / `managerNamespace` | Traffic manager 所在 namespace |
| `WorkloadNamespace` / `workloadNamespace` | 用户工作负载 namespace |
| RPC `req.Namespace` | 始终是工作负载 namespace |
| RPC `req.ManagerNamespace` | 始终是 manager namespace |

## 依赖规则

```
pkg/daemon/action  → pkg/handler (via Connection interface)
pkg/daemon/grpcutil → pkg/daemon/rpc
pkg/handler        → pkg/config, pkg/util, pkg/core, pkg/inject, pkg/dns, pkg/tun
pkg/handler        → pkg/daemon/rpc (仅 gRPC client: TunConfigService)
pkg/ssh            → pkg/config, pkg/util (不依赖 daemon/rpc)
pkg/util           → pkg/config (不依赖上层包)
pkg/controlplane   → pkg/dhcp, pkg/daemon/rpc (server 端 DHCP + TunConfigService)
```

**Client 侧不再依赖 pkg/dhcp** — IP 分配完全由 server 端 TunConfigService 管理。

## 文件布局

```
pkg/handler/
├── connect.go            ConnectOptions, RentIP, DoConnect, getCIDR
├── connect_tun.go        Run() server runner, healthCheck helpers
├── connect_dns.go        detectNameserver helpers
├── connect_route.go      newTickerResetHandler
├── connect_upgrade.go    upgradeDeploy
├── network.go            NetworkManager (Start/Stop/ChangeTunIP/IPWatcher)
├── proxy_manager.go      ProxyManager (Add/Remove/Leave)
├── configmap_store.go    ConfigMapStore (Get/Set/Health)
├── proxy.go              Proxy/ProxyList 数据类型
├── proxy_mapper.go       Mapper (port-forward 配置监听)
├── k8s_client.go         K8sClient 嵌入结构
├── connection.go         Connection 接口定义
├── healthchecker.go      HealthStatus 类型
├── cleaner.go            Cleanup, 信号处理
├── sync.go               SyncOptions, DoSync
├── traffmgr.go           Traffic manager pod 创建
├── traffmgr_resources.go K8s 资源生成器
├── leave.go              代理卸载委托
├── reset.go              重置工作负载
├── once.go               Server 端辅助 (labelNs, genTLS, getCIDR)
├── sort.go               Connects 排序
├── extraoptions.go       ExtraRouteInfo + RPC 转换
├── sshconv.go            SSH config ↔ RPC 转换
└── testing.go            测试辅助
```
