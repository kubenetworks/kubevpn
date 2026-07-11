# KubeVPN 全面重构计划

## 项目现状概览

| 维度 | 数据 |
|------|------|
| 总源码行数（pkg/, 非测试/非生成） | ~23,500 行 |
| 包数量 | ~30 个 |
| 最大文件 | `ssh/gssapi_ccache.go` (668), `controlplane/cache.go` (650), `handler/sync.go` (543) |
| 最大包 (文件数) | `util/` (24), `core/` (22), `daemon/action/` (20), `handler/` (19) |
| 接口数 | 14 个 |
| TODO/FIXME | 5 处（envoy.go 的 context.TODO, ssh/config.go, dhcp, cp, dns_linux） |

---

## 第一部分：项目架构理解

### 数据流概览

```
用户 CLI (cmd/kubevpn/cmds/)
    │
    ▼
User Daemon (daemon/action/Server, IsSudo=false)  ← 控制面
    │   ↑ gRPC
    ▼   │
Sudo Daemon (daemon/action/Server, IsSudo=true)   ← 数据面
    │
    ▼
handler.ConnectOptions.DoConnect()
    ├── getCIDR()           → 检测集群 Pod/Service CIDR
    ├── createOutboundPod() → 部署 traffic manager (Deployment + Service + RBAC + Webhook)
    ├── upgradeDeploy()     → 升级已有的 traffic manager
    ├── portForward()       → kubectl port-forward 到 traffic manager pod
    ├── startLocalTunServer() → 创建 TUN 设备 + gvisor 网络栈
    ├── addRouteDynamic()   → 动态路由 (informer 监听 Pod/Service IP 变化)
    └── setupDNS()          → 配置 DNS 服务
```

### ConnectOptions 双角色模式

`ConnectOptions` 在两个 daemon 层有不同初始化：

| | User Daemon (控制面) | Sudo Daemon (数据面) |
|---|---|---|
| 初始化路径 | `redirectConnectToSudoDaemon` | `Connect` (IsSudo=true) |
| 负责什么 | SSH jump、DHCP IP 租赁、健康检查、proxy/sync 状态 | TUN 设备、路由、DNS、端口转发 |
| ctx | nil (不创建 cancel context) | 非 nil (通过 DoConnect 创建) |
| Cleanup 行为 | 释放 IP、leave proxy、rollback | cancel DNS、cancel context、rollback |

### Sidecar 注入策略模式

```
NewInjector(opts) → Injector 接口
    ├── vpnInjector:     Service 之外, 无 headers/portMaps → 仅注入 VPN 容器
    ├── meshInjector:    有 headers 或 portMaps → 注入 VPN + Envoy 容器
    └── fargateInjector: 目标是 K8s Service → 注入 Envoy + SSH 容器
```

---

## 第二部分：问题清单与重构项

### 模块 1: `pkg/util/` — 万能工具包拆分

**问题**: 24 个文件、~3,500 行代码，职责严重混杂。包含 K8s 操作、网络工具、Docker 操作、文件下载、gRPC 工具、TLS、版本管理等完全无关的功能。

**重构方案**:

| 原文件 | 迁移到 | 理由 |
|--------|--------|------|
| `cidr.go` (471行) | `pkg/network/cidr.go` | 纯网络 CIDR 逻辑，与 K8s 无关 |
| `net.go` (258行) | `pkg/network/device.go` | TUN 设备发现、IP 解析、ICMP |
| `gvisor.go` (76行) | `pkg/network/gvisor.go` | gvisor 工具 |
| `pod.go` (364行) | 保留在 `util/` | 太多调用者，迁移成本高 |
| `docker.go` (220行) | `pkg/docker/` | Docker CLI 操作独立包 |
| `kubeconfig.go` (259行) | 保留在 `util/` | K8s 客户端初始化，核心依赖 |
| `unstructured.go` (223行) | 保留在 `util/` | K8s 资源操作 |
| `file.go` (135行) | 保留在 `util/` | 通用文件操作 |
| `grpc.go` (132行) | 保留在 `util/` | gRPC stream 工具 |

**具体步骤**:
1. 创建 `pkg/network/` 包，迁移 `cidr.go`、`net.go`、`gvisor.go`
2. 创建 `pkg/docker/` 包，迁移 `docker.go`（仅 `pkg/run/` 引用）
3. 使用 `git mv` 保留历史
4. 更新所有 import path
5. 验证: `go build ./...` + `go vet ./...`

**估计工作量**: 中等，~30 个文件需要更新 import

---

### 模块 2: `pkg/handler/connect.go` — ConnectOptions God Object 拆分

**问题**: `ConnectOptions` 结构体有 22+ 个字段，承担了太多职责：
- DHCP IP 管理
- K8s 客户端初始化
- 路由管理
- DNS 配置
- 健康检查
- Proxy 工作负载管理
- ConfigMap 信息读写
- TUN 设备管理
- 清理/回滚
- 信号处理

**当前结构体字段**:
```go
type ConnectOptions struct {
    // 9 个公开字段 (配置)
    // 13+ 个私有字段 (运行时状态)
    // 方法分散在 7 个文件中
}
```

**重构方案**: 将 ConnectOptions 的关注点分离为内嵌的子组件

```go
// Phase 1: 提取独立关注点为子结构体
type ConnectOptions struct {
    // 配置 (不变)
    ManagerNamespace     string
    ExtraRouteInfo       ExtraRouteInfo
    OriginKubeconfigPath string
    OriginNamespace      string
    Image                string
    ImagePullSecretName  string
    Request              *rpc.ConnectRequest

    // 子组件 (新)
    k8s      *k8sClient     // clientset, restclient, config, factory
    tunnel   *tunnelState   // network (*NetworkManager), tunName, cidrs
    routing  *routeManager  // addRoute, addRouteDynamic, apiServerIPs
    health   *healthChecker // HealthPeriod, HealthCheckOnce, healthStatus
    cleanup  *cleanupStack  // rollbackFuncList, once, cancel
    proxies  ProxyList
    dns      *dns.Config
    Sync     *SyncOptions
}
```

**但考虑到项目指令 "不要碰 cmd/"**，而 ConnectOptions 在 daemon/action 中被直接构造和存储在 `svr.connections` 切片中，改动公开 API 风险较高。所以采用更保守的策略：

**实际步骤**:
1. **提取 `k8sClient` 内部结构** — 将 `clientset`, `restclient`, `config`, `factory` 打包，减少 ConnectOptions 字段数
2. **提取 `tunnelState`** — 已通过 `NetworkManager` 实现（持有 localTunIPv4/v6, tunName 等网络状态）
3. **保持公开 API 不变** — 只改内部组织
4. 验证: `go build ./...` + `go test ./pkg/handler/...`

**估计工作量**: 大，触及 handler/ 所有文件 + daemon/action/ 多个文件

---

### 模块 3: `pkg/handler/connect_tun.go` — portForward 函数过长

**问题**: `portForward` 函数约 90 行，包含：
- 嵌套的匿名函数循环重连
- 多个 goroutine 启动 (健康检查、pod 状态监控)
- 复杂的 first/非-first 分支逻辑
- 字符串错误匹配 (`strings.Contains(err.Error(), "unable to listen...")`)

**重构方案**:
1. 提取 `portForwardOnce()` — 单次端口转发尝试
2. 提取 `waitForFirstReady()` — 首次就绪等待逻辑
3. 将字符串错误匹配改为类型断言或 sentinel error
4. `healthCheckPortForward` 和 `healthCheckTCPConn` 有大量重复模式 — 提取公共 `healthCheckLoop(checker func() error)`

**具体步骤**:
```
1. 提取 portForwardOnce(ctx, portPair) → (readyChan, errChan)
2. 提取 healthCheckLoop(ctx, interval, checker) 公共函数
3. 重写 healthCheckPortForward 和 healthCheckTCPConn 使用 healthCheckLoop
4. 验证: go build + go test ./pkg/handler/...
```

**估计工作量**: 小-中

---

### 模块 4: `pkg/handler/traffmgr.go` — createOutboundPod 资源创建顺序重构

**问题**: `createOutboundPod` 181 行，线性创建 7 种 K8s 资源，每个都有独立的错误处理和日志。模式重复但无法循环（资源类型不同）。

**重构方案**: 这是 **固有复杂度** — 不应强行抽象。但可以：
1. 将 `deleteResource` 闭包提取为独立方法 `cleanupTrafficManagerResources(ctx, clientset, ns)`
2. 这个函数与 `once.go` 中的 `genTLS`, `labelNs` 有重复逻辑 — 可以共享

**估计工作量**: 小

---

### 模块 5: `pkg/inject/envoy.go` — context.TODO() 消除

**问题**: 4 处 `context.TODO()` 用于 K8s API 调用，应该传入 `ctx` 参数。

**重构方案**:
1. `addEnvoyConfig` 函数签名添加 `ctx context.Context` 参数
2. `removeEnvoyConfig` 函数签名添加 `ctx context.Context` 参数
3. 更新所有调用者 (vpn.go, mesh.go, fargate.go, mesh.go/UnpatchContainer)

**具体步骤**:
```
1. addEnvoyConfig(ctx, mapInterface, ...) — 添加 ctx
2. removeEnvoyConfig(ctx, mapInterface, ...) — 添加 ctx
3. 更新 vpnInjector.Inject, meshInjector.Inject, fargateInjector.Inject
4. 更新 UnpatchContainer
5. 验证: go build + go vet
```

**估计工作量**: 小

---

### 模块 6: `pkg/daemon/action/` — 消除重复的 logger/session 设置模式

**问题**: 几乎每个 RPC handler 都重复这个模式：
```go
req, err := resp.Recv()
logger := plog.GetLoggerForClient(req.Level, io.MultiWriter(newStreamWriter(...), svr.LogFile))
ctx := plog.WithLogger(resp.Context(), logger)
```

Connect, Disconnect, Proxy, Sync, Leave, Reset, Uninstall 全都有类似的初始化模式。

**重构方案**: 提取 `initStreamAction` 辅助函数

```go
func initStreamAction[Req any, Resp any](
    svr *Server,
    stream interface{ Recv() (*Req, error) },
    logLevel func(*Req) int32,
    sendMsg func(string) error,
) (*Req, *log.Logger, context.Context, error)
```

**但考虑到**: 每个 handler 的 Req/Resp 类型都不同，Go 泛型在 gRPC stream 上不好用。更实际的方案是保持现状但确保模式一致。

**实际步骤**:
1. 统一所有 handler 的 logger 创建为 `plog.GetLoggerForClient` (有些用 `int32(log.InfoLevel)` 硬编码)
2. 确保所有 handler 都通过 `newStreamWriter` 创建 writer
3. 不做强制抽象 — 模式相似但每个 handler 有微妙差异

**估计工作量**: 小

---

### 模块 7: `pkg/handler/sync.go` — 543 行巨型文件拆分

**问题**: `SyncOptions.DoSync` 方法约 200 行，包含了：
- 工作负载操作 (get controller, scale replica)
- Syncthing 配置生成
- Pod 创建和等待
- 环境变量收集
- 远程目录解析

**重构方案**:
1. 提取 `preparePodSpec(ctx) (*v1.PodTemplateSpec, error)` — 处理 pod spec 构建
2. 提取 `configureSyncthing(ctx, pod) error` — syncthing 配置
3. 将 `ConvertApiServerToNodeIP` 和底部的 helper 函数移到独立文件 `sync_helpers.go`

**具体步骤**:
```
1. sync.go 拆为 sync.go (SyncOptions + DoSync 主流程) + sync_helpers.go (工具方法)
2. DoSync 内部提取 2-3 个子方法
3. 验证: go build + go test
```

**估计工作量**: 中

---

### 模块 8: `pkg/ssh/gssapi_ccache.go` — 668 行最大文件

**问题**: 这是 GSSAPI Kerberos ccache 解析的实现，668 行全是协议解析逻辑。

**评估**: 这是 **固有复杂度**，协议格式复杂，不应拆分。但可以：
1. 检查是否有开源库可以替代（`gopkg.in/jcmturner/gokrb5.v7/credentials`）
2. 如果功能与开源库重复，考虑替换

**估计工作量**: 需调研，可能大

---

### 模块 9: `pkg/controlplane/cache.go` — 650 行

**问题**: Envoy xDS 配置缓存实现。

**评估**: 同上，固有复杂度。Envoy xDS snapshot 管理本身就复杂。不建议强行重构。

---

### 模块 10: `pkg/util/cidr.go` — CIDR 检测逻辑优化

**问题**: 471 行，包含多种 CIDR 检测策略：
- `GetCIDRByDumpClusterInfo` — 从 cluster-info dump 解析
- `GetCIDRFromCNI` — 创建特殊 Pod 检测 CNI
- `GetServiceCIDRByCreateService` — 创建无效 Service 获取错误消息
- `GetPodCIDRFromPod` — 从已有 Pod 的 IP 推断

**重构方案**:
1. 每个策略提取为独立函数（已完成）
2. `GetCIDR` 主函数改为 strategy chain 模式，更清晰的 fallback 逻辑
3. 将 CIDR 工具函数 (`RemoveCIDRsContainingIPs`, `RemoveLargerOverlappingCIDRs`) 与检测逻辑分离

**估计工作量**: 中

---

### 模块 11: `pkg/handler/cleaner.go` — Cleanup 逻辑清理

**问题**: `Cleanup` 方法通过 `c.ctx != nil` 来判断是 user daemon 还是 sudo daemon，这是隐式约定。

**重构方案**:
1. 添加显式字段 `isDataPlane bool` 到 ConnectOptions
2. 用 `c.isDataPlane` 替代 `c.ctx != nil` 判断
3. 统一 cleanup 中重复的 rollback 执行循环

**具体步骤**:
```go
// Before:
var userDaemon = true
if c.ctx != nil {
    userDaemon = false
}

// After:
// c.isDataPlane 在 DoConnect() 入口处设为 true
```

**估计工作量**: 小

---

### 模块 12: Direct logrus 导入统一

**问题**: 15 个源文件直接导入 `log "github.com/sirupsen/logrus"`，而项目规范要求使用 `plog` alias。

**分析**: 这些导入主要用于 `*log.Logger` 类型引用（如函数参数），而非直接调用 logrus API。但仍应统一。

**重构方案**:
1. 对于仅用类型引用的文件，改为 `plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"` 并用 `plog.Logger` 类型
2. 但 `plog` 包本身就是对 logrus 的封装，`plog.Logger` 可能不存在
3. 实际方案: 保留 `log "github.com/sirupsen/logrus"` 用于类型引用，这是合理的

**估计工作量**: 极小或不做

---

### 模块 13: `pkg/handler/proxy.go` — Mapper 与 Proxy 分离

**问题**: `proxy.go` 325 行，混合了两个不同关注点：
- `ProxyList` / `Proxy` — 代理工作负载列表管理
- `Mapper` — SSH 反向隧道管理，包含 informer watch、reconciliation 逻辑

**重构方案**:
1. 将 `Mapper` 及其方法 (`Run`, `reconcilePodsFromInformer`, `getPortMappingFromCache`, `extractPortMapping`, `startTunnels`, `cancelAllTunnels`) 移到 `proxy_mapper.go`
2. `proxy.go` 保留 `Proxy`, `ProxyList`, `Resources`

**具体步骤**:
```
1. 创建 proxy_mapper.go，移入 Mapper 相关代码
2. proxy.go 保留 ~90 行
3. 验证: go build
```

**估计工作量**: 小

---

### 模块 14: `pkg/daemon/action/status.go` — genStatus/gen 函数命名改进

**问题**: 
- `genStatus` 和 `gen` 函数名过于模糊
- `gen` 函数返回 3 个值 `(proxyList, syncList, error)`，职责不清

**重构方案**:
1. `genStatus` → `buildConnectionStatus`
2. `gen` → `buildProxyAndSyncStatus`
3. `useSecondPort` → `extractLocalPorts`

**估计工作量**: 极小

---

### 模块 15: `pkg/daemon/action/connect.go` — forwardConnectToSudo 参数过多

**问题**: `forwardConnectToSudo` 有 9 个参数。

**重构方案**: 提取 `sudoForwardArgs` 结构体：
```go
type sudoForwardArgs struct {
    req            *rpc.ConnectRequest
    connect        *handler.ConnectOptions
    resp           rpc.Daemon_ConnectServer
    cli            rpc.DaemonClient
    connResp       *grpc.BidiStreamingClient[...]
    kubeconfigPath string
    connectionID   string
    logger         *log.Logger
}
```

**评估**: 这个函数只有一个调用者，抽结构体反而增加复杂度。**不做**。

---

### 模块 16: 测试覆盖补充

**现状分析**:
- `pkg/core/` — 有 datapath_test.go (1183行)、integration_tun_test.go (1051行) 等，覆盖较好
- `pkg/handler/` — 仅有 testing.go (辅助)，缺乏单元测试
- `pkg/inject/` — 有 envoy_config_test.go
- `pkg/daemon/action/` — 有 persistence_test.go、action_test.go、status_test.go
- `pkg/util/` — 有多个测试文件，覆盖较好
- `pkg/localproxy/` — 有 server_test.go

**需要补充测试的模块**:
1. `pkg/handler/proxy.go` — Mapper 的 reconciliation 逻辑
2. `pkg/handler/connect_route.go` — `addRoute`, `parseServiceHost` 路由计算
3. `pkg/handler/leave.go` — LeaveResource 逻辑
4. `pkg/inject/injector.go` — NewInjector 工厂选择逻辑

---

## 第三部分：重构优先级排序

### 第一优先级 — 低风险高收益

| # | 模块 | 预计改动量 | 风险 | 收益 |
|---|------|-----------|------|------|
| 5 | inject/envoy.go context.TODO() | ~10行 | 极低 | 修复潜在取消传播问题 |
| 11 | cleaner.go 显式字段 | ~20行 | 低 | 消除隐式约定 |
| 13 | proxy.go 拆分 Mapper | ~0行新增，拆文件 | 极低 | 提高可读性 |
| 14 | status.go 函数重命名 | ~10行 | 极低 | 提高可读性 |

### 第二优先级 — 中等风险中等收益

| # | 模块 | 预计改动量 | 风险 | 收益 |
|---|------|-----------|------|------|
| 3 | connect_tun.go portForward 重构 | ~60行 | 中 | 消除 90 行长函数 |
| 4 | traffmgr.go cleanup 提取 | ~30行 | 低 | 消除重复 |
| 6 | daemon/action logger 统一 | ~30行 | 低 | 一致性 |
| 7 | sync.go 拆分 | ~0行新增，拆文件+方法 | 中 | 543→~250+~200 |

### 第三优先级 — 高风险高收益

| # | 模块 | 预计改动量 | 风险 | 收益 |
|---|------|-----------|------|------|
| 1 | util/ 拆包 | ~100行 import 改动 | 高 | 包职责清晰 |
| 2 | ConnectOptions 子结构提取 | ~200行 | 高 | God object 拆解 |
| 10 | cidr.go 策略链 | ~100行 | 中 | 可读性和可维护性 |

### 第四优先级 — 补充测试

| # | 模块 | 预计行数 |
|---|------|---------|
| 16a | handler/proxy_test.go | ~200行 |
| 16b | handler/connect_route_test.go | ~150行 |
| 16c | inject/injector_test.go | ~100行 |

---

## 第四部分：执行建议

### 每轮重构的验证清单

```bash
go build ./...              # 编译通过
go vet ./pkg/...            # 静态分析
go test ./pkg/...           # 所有测试通过
git diff --stat             # 确认只改了预期文件
```

### 约束条件

1. **绝不修改 `cmd/`** — CLI 冻结
2. **绝不修改 `pkg/daemon/rpc/*.pb.go`** — 自动生成
3. **绝不修改 `pkg/syncthing/auto/gui.files.go`** — 嵌入资源
4. **保持公开 API 兼容** — `ConnectOptions` 的公开字段和方法签名不变
5. **每次提交一个逻辑变更** — 方便 code review
6. **使用 `git mv`** 保留文件历史

### 推荐执行顺序

```
Round 1: #5 + #11 + #13 + #14  (半天, 热身)
Round 2: #3 + #4 + #6          (1天, 中等改动)
Round 3: #7                     (半天, 文件拆分)
Round 4: #1                     (1天, util拆包)
Round 5: #2                     (1-2天, ConnectOptions重构)
Round 6: #16                    (1天, 补充测试)
Round 7: #10                    (半天, CIDR重构)
```

**总计预计工作量: 5-7 天**
