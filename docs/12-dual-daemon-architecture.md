# KubeVPN 双 Daemon 架构文档

## 为什么需要这份文档

KubeVPN 使用双进程架构（User Daemon + Root Daemon），两个进程各自持有**独立的 `ConnectOptions` 实例**，但字段含义和使用场景完全不同。过去因为缺乏这份架构文档，出现过在错误的 daemon 中初始化字段的 bug（如 OwnerID 在 Root Daemon 中无用生成）。

**任何修改 `ConnectOptions` 字段或 `daemon/action/` 代码的人，必须先阅读本文档。**

---

## 1. 架构总览

```
┌─────────────────────────────────────────────────────┐
│                     用户 CLI                         │
│              cmd/kubevpn/cmds/                       │
└──────────────────────┬──────────────────────────────┘
                       │ gRPC (unix socket)
                       ▼
┌─────────────────────────────────────────────────────┐
│              User Daemon（用户态，无 root）            │
│              pkg/daemon/action/ (IsSudo=false)        │
│                                                       │
│  职责：                                               │
│  ├── SSH 跳板（resolveKubeconfig）                    │
│  ├── Traffic Manager 创建/升级（CreateOutboundPod/UpgradeDeploy）│
│  ├── IP 分配（RentIP → TunConfigService gRPC）        │
│  ├── 代理注入（CreateRemoteInboundPod → inject/）      │
│  ├── 文件同步（DoSync）                               │
│  ├── 健康检查（HealthPeriod）                         │
│  ├── 连接状态持久化（OffloadToConfig/LoadFromConfig）  │
│  └── OwnerID 生成和管理                               │
│                                                       │
│  持有：自己的 ConnectOptions（控制面）                  │
│  存储：svr.connections 切片                            │
└──────────────────────┬──────────────────────────────┘
                       │ gRPC (unix socket, sudo)
                       ▼
┌─────────────────────────────────────────────────────┐
│              Root Daemon（root 权限）                 │
│              pkg/daemon/action/ (IsSudo=true)         │
│                                                       │
│  职责：                                               │
│  ├── TUN 设备创建和管理                               │
│  ├── 路由表操作（addRoute, addRouteDynamic）           │
│  ├── DNS 配置（setupDNS, CancelDNS）                  │
│  ├── 端口转发（portForward 到 traffic manager pod）   │
│  ├── gvisor 网络栈运行                                │
│  └── 信号处理和清理                                   │
│                                                       │
│  持有：自己的 ConnectOptions（数据面）                  │
│  存储：svr.connections 切片                            │
└─────────────────────────────────────────────────────┘
```

## 2. ConnectOptions 双实例

**关键规则：User Daemon 和 Root Daemon 各自创建独立的 `ConnectOptions` 实例。它们不共享内存。**

### 2.1 User Daemon 的 ConnectOptions（控制面）

创建位置：`daemon/action/connect_elevate.go` → `redirectConnectToSudoDaemon()`

```go
connect := &handler.ConnectOptions{
    ManagerNamespace:     req.Namespace,
    WorkloadNamespace:    req.Namespace,
    ExtraRouteInfo:       ...,
    OriginKubeconfigPath: req.OriginKubeconfigPath,
    RequestRaw:           reqBytes,
    OwnerID:              uuid.New().String()[:12],  // ← 只在这里生成
    Image:                req.Image,                 // ← 用于 CreateOutboundPod
    ImagePullSecretName:  req.ImagePullSecretName,   // ← 用于 CreateOutboundPod
}
```

使用的字段：
| 字段 | 用途 |
|------|------|
| `K8sClient` (clientset, factory) | 操作 K8s API（注入 sidecar、查询 ConfigMap） |
| `OwnerID` | 写入 Envoy Rule 标识所有权 |
| `Image/ImagePullSecretName` | CreateOutboundPod 创建 traffic manager pod |
| `proxyWorkloads` | 跟踪当前代理的工作负载 |
| `healthStatus` | 定期健康检查 |
| `Sync` | 文件同步选项 |
| `RequestRaw` | 持久化时需要 |
| `LocalTunIPv4/v6` | RentIP 分配，通过 gRPC metadata 传给 Root Daemon |

**不使用的字段（在 User Daemon 中始终为零值）**：
| 字段 | 原因 |
|------|------|
| `ctx/cancel` | 不调用 DoConnect，不设置这些 |
| `isDataPlane` | 始终 false |
| `tunName` | 不创建 TUN 设备 |
| `dnsConfig` | 不配置 DNS |
| `cidrs` | 不检测 CIDR（Root Daemon 做） |
| `apiServerIPs` | 不做路由过滤 |

### 2.2 Root Daemon 的 ConnectOptions（数据面）

创建位置：`daemon/action/connect.go` → `Connect()` (IsSudo=true 分支)

```go
connect := &handler.ConnectOptions{
    ManagerNamespace:     req.ManagerNamespace,
    ExtraRouteInfo:       ...,
    OriginKubeconfigPath: req.OriginKubeconfigPath,
    WorkloadNamespace:    req.Namespace,
    Lock:                 &svr.Lock,
    Image:                req.Image,
    ImagePullSecretName:  req.ImagePullSecretName,
    RequestRaw:           reqBytes,
    // 注意：没有 OwnerID — Root Daemon 不操作 Envoy 配置
    // 注意：不调用 CreateOutboundPod/UpgradeDeploy — 那是控制面职责
}
```

使用的字段：
| 字段 | 用途 |
|------|------|
| `ctx/cancel` | DoConnect 创建，控制数据面生命周期 |
| `isDataPlane` | DoConnect 设为 true |
| `network` | NetworkManager：TUN、端口转发、路由、DNS |
| `configMapStore` | ConfigMap informer 用于 CIDR 缓存 |

**不使用的字段（在 Root Daemon 中无意义）**：
| 字段 | 原因 |
|------|------|
| `OwnerID` | 不操作 Envoy 配置 |
| `proxyWorkloads` | 不管理代理工作负载 |
| `healthStatus` | 不做 Envoy 健康检查（User Daemon 做） |
| `Sync` | 文件同步在 User Daemon |

## 3. 操作流程

### 3.1 Connect 流程

```
CLI: kubevpn connect
  │
  ▼
User Daemon: Connect RPC
  ├── redirectConnectToSudoDaemon()
  │     ├── 创建 ConnectOptions（控制面, 含 OwnerID, Image）
  │     ├── resolveKubeconfig（SSH 跳板）
  │     ├── InitClient
  │     ├── detectAndSetManagerNamespace
  │     ├── forwardConnectToSudo()
  │     │     ├── CreateOutboundPod（创建 traffic manager pod）
  │     │     ├── UpgradeDeploy（升级 traffic manager）
  │     │     ├── RentIP（通过 TunConfigService 分配 TUN IP）
  │     │     ├── 转发 req 到 Root Daemon ──────────┐
  │     │     ├── 等待 Root Daemon 完成               │
  │     │     ├── 启动 HealthPeriod                   │
  │     │     └── 存入 svr.connections                │
  │                                                   ▼
  │                                    Root Daemon: Connect RPC
  │                                      ├── 创建 ConnectOptions（数据面）
  │                                      ├── GetIPFromContext（从 gRPC header 取 IP）
  │                                      ├── DoConnect()
  │                                      │     ├── getCIDR
  │                                      │     ├── NetworkManager.Start()
  │                                      │     │     ├── portForward
  │                                      │     │     ├── startTUN
  │                                      │     │     ├── AddRouteDynamic
  │                                      │     │     └── setupDNS
  │                                      │     └── StartIPWatcher
  │                                      └── 存入 svr.connections
  ▼
User Daemon: 返回成功给 CLI
```

### 3.2 Proxy 流程（只在 User Daemon）

```
CLI: kubevpn proxy deploy/web --headers version=v1
  │
  ▼
User Daemon: Proxy RPC
  ├── Connect（同上流程）
  ├── findConnection(connectionID)  ← 找到 User Daemon 的 ConnectOptions
  └── CreateRemoteInboundPod()
        ├── inject.NewInjector(InjectOptions{OwnerID: c.OwnerID})
        │     └── addEnvoyConfig(..., ownerID)
        │           └── Rule{OwnerID: ownerID}  ← 写入 ConfigMap
        └── 启动 Mapper（SSH 隧道）
```

### 3.3 Disconnect 流程

```
CLI: kubevpn disconnect
  │
  ▼
User Daemon: Disconnect RPC
  ├── 转发到 Root Daemon ──────────────┐
  │                                     ▼
  │                            Root Daemon: Disconnect RPC
  │                              └── cleanupDataPlane()
  │                                    ├── rollback functions
  │                                    ├── CancelDNS
  │                                    └── cancel context → TUN/路由停止
  │
  └── cleanupControlPlane()
        ├── ReleaseIP（归还 DHCP）
        ├── 删除临时 Pod/Job
        ├── LeaveAllProxyResources（清理 Envoy Rules, 用 OwnerID 匹配）
        └── rollback functions
```

## 4. 修改 ConnectOptions 的规则

### 规则 1：新增字段前先确定属于哪个 daemon

问自己：这个字段是被 `DoConnect` 使用（数据面），还是被 `CreateRemoteInboundPod`/`LeaveResource`/`HealthPeriod` 使用（控制面）？

- **数据面**：不需要 json tag，不需要持久化，不需要在 `redirectConnectToSudoDaemon` 中设置
- **控制面**：需要 json tag（用于持久化），需要在 `redirectConnectToSudoDaemon` 中初始化

### 规则 2：不要在 DoConnect 中初始化控制面字段

`DoConnect` 只在 Root Daemon 运行。任何只被 User Daemon 使用的字段（如 OwnerID），在 DoConnect 中初始化是**无用代码**——Root Daemon 的 ConnectOptions 和 User Daemon 的是不同的对象。

### 规则 3：持久化只在 User Daemon

`OffloadToConfig` 在 `!svr.IsSudo` 路径中调用。只有 User Daemon 的 ConnectOptions 被序列化。需要跨重启保留的字段必须有 `json:` tag。

### 规则 4：不要假设两个 daemon 的 ConnectOptions 共享状态

它们通过 gRPC 通信，不共享内存。任何需要跨 daemon 传递的数据必须通过 `rpc.ConnectRequest`/`rpc.ConnectResponse` 传递。

## 5. 常见错误示例

### ❌ 错误：在 DoConnect 中初始化只有控制面使用的字段

```go
func (c *ConnectOptions) DoConnect(ctx context.Context) (err error) {
    c.OwnerID = uuid.New().String()[:12]  // 无用！Root Daemon 不用 OwnerID
    ...
}
```

### ✅ 正确：在 User Daemon 构造 ConnectOptions 时初始化

```go
// daemon/action/connect.go → redirectConnectToSudoDaemon()
connect := &handler.ConnectOptions{
    ...
    OwnerID: uuid.New().String()[:12],  // User Daemon 使用 OwnerID
}
```

### ❌ 错误：在 redirectConnectToSudoDaemon 中设置数据面字段

```go
connect := &handler.ConnectOptions{
    ...
    tunName: "utun0",  // 无用！User Daemon 不创建 TUN
}
```

### ✅ 正确：让 DoConnect 设置数据面字段

```go
func (c *ConnectOptions) DoConnect(ctx context.Context) (err error) {
    ...
    c.network.Start(ctx)  // Root Daemon: 启动完整网络栈 (port-forward, TUN, routes, DNS)
}
```
