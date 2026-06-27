# KubeVPN 项目设计问题分析报告

## 一、总体架构

### 1.1 分层架构（整体合理，有违规）

```
cmd/kubevpn/cmds/  ← CLI 层（应该只做 flag 解析 + RPC 调用）
    ↓
pkg/daemon/        ← Daemon 层（gRPC server，双 daemon 架构）
    ↓
pkg/handler/       ← 业务逻辑层（连接、代理、同步编排）
    ↓
pkg/inject/        ← Sidecar 注入策略
pkg/core/          ← 网络协议核心（TUN, gvisor）
pkg/dns/           ← DNS 配置
pkg/tun/           ← TUN 设备管理
```

**问题**：
- `cmd/` 直接导入了 17 个 `pkg/` 内部包（包括 `core`, `dns`, `dhcp`, `inject`），绕过了 daemon/handler 编排层
- `daemon/action` 也直接导入 `core`, `dns`, `tun`，与 handler 形成两个并行编排路径
- 建议：`cmd/` 应只导入 `pkg/daemon` 和 `pkg/config`

### 1.2 双 Daemon 架构（合理但实现粗糙）

- **设计意图**：User daemon（用户态）处理 SSH 跳板和 kubeconfig；Sudo daemon（root）处理 TUN 设备和路由
- **问题**：每个 action handler 手工编写 gRPC 转发逻辑，无统一的"forward-to-other-daemon"抽象。Connect/Proxy/Sync 各自实现了 connResp 同步、cancel 转发、logger 管道，代码重复且易错（之前修了 3 个 race condition）

---

## 二、核心数据结构设计问题

### 2.1 ConnectOptions God Object（22+ 字段）

混合了 5 个关注点：
| 关注点 | 字段 |
|--------|------|
| K8s 客户端 | clientset, restclient, config, factory |
| 网络标识 | LocalTunIPv4/v6, tunName, cidrs, dhcp |
| 生命周期 | ctx, cancel, once, rollbackFuncList, isDataPlane |
| DNS | dnsConfig, extraHost |
| 代理管理 | proxyWorkloads, healthStatus, cmInformer |

**与 SyncOptions 重复**：clientset, config, factory, rollbackFuncList, AddRollbackFunc(), InitClient() 完全重复。且 ConnectOptions 用 `kubernetes.Interface`，SyncOptions 用 `*kubernetes.Clientset`（不一致）。

**建议**：提取 `K8sClientBundle` 嵌入结构，两者共享。

### 2.2 Cleanup 双路径（isDataPlane bool）

`Cleanup()` 方法根据 `isDataPlane` 走完全不同的路径。User daemon 释放 DHCP + 删 Pod + leave proxy + rollback。Sudo daemon 做 rollback + 取消 DNS + cancel context。应该拆为两个独立的 cleanup 实现，而非靠 bool 分支。

### 2.3 Rollback 函数列表（脆弱）

- 无命名（不知道回滚什么）
- 无顺序保证（可能有依赖）
- 全部 swallow error（难以调试）
- 建议：改为命名清理注册表 `map[string]func() error`

---

## 三、Inject/Controlplane 设计

### 3.1 Envoy 配置存储（ConfigMap YAML）

- **无 schema 版本号**：Virtual/Rule 结构变更时，旧 ConfigMap 静默反序列化为零值（FargateMode fallback 就是此问题的证据）
- **单 key 存所有工作负载**：每次更新序列化全部 Virtual 列表，多用户并发代理时成为写瓶颈
- **PortMap 用字符串编码两个值**：`map[int32]string` 中 string 是 `"envoyPort:localPort"`，需要 `strings.Cut` 解析——应改为结构体

### 3.2 Rule 无所有者标识

Rule 的所有权通过匹配 `LocalTunIPv4` 推断（非 Fargate 模式）或匹配 headers（Fargate 模式）。IP 重新分配后，旧 Rule 变成孤儿。建议增加显式 `OwnerID` 字段。

### 3.3 VPN-only 模式写 Envoy 配置

`vpnInjector` 调用 `addEnvoyConfig` 写入空 headers 的 Rule——即使 VPN 模式不需要 Envoy 路由。这耦合了 VPN 模式与控制面。

---

## 四、核心网络层设计

### 4.1 DefaultRouteHub 全局单例

`route.go` 中 `var DefaultRouteHub = NewRouteHub()` 是进程级共享可变状态。阻止了在同一进程中运行多个独立 VPN 会话，也使测试困难。应强制调用者显式传入 RouteHub。

### 4.2 ParseForwarder 忽略协议字段

`ParseForwarder` 解析 URI 的 scheme（如 `tcp://`）但不使用——始终返回 `UDPOverTCPConnector` + `TCPTransporter`。Forwarder 抽象假装协议无关但只支持一种传输。

### 4.3 IP 键的热路径分配

RouteHub 用 `string(net.IP)` 作 map key，每次查找都有堆分配。改用 `[16]byte` 固定大小数组可零分配。

---

## 五、util/ 万能包

42 个文件、5300+ 行，混合了完全无关的领域：
- CIDR 检测（K8s 集群探测）
- Docker 容器生命周期
- TLS 证书生成
- Helm 操作
- gRPC stream 工具
- kubeconfig 操作
- 文件下载

`util/` 甚至向上依赖 `pkg/daemon/rpc` 和 `pkg/driver`，违反了工具包的分层原则。

**建议拆分**：
| 现有文件 | 目标包 |
|---------|--------|
| cidr.go | `pkg/netutil/` |
| docker.go | `pkg/dockerutil/` |
| tls.go | `pkg/tlsutil/` |
| helm.go | `pkg/helmutil/` |

---

## 六、config/ 混用常量与运行时状态

`config.go` 在 `init()` 中解析 CIDR 字符串为 `*net.IPNet` 并存储为包级 var。这是伪装成配置的全局可变状态。如果 CIDR 字符串格式错误，`init()` 会 panic——一个隐藏的运行时风险。

建议：CIDR 字符串保持为 const，解析推迟到使用时或通过构造函数传入。

---

## 七、优先级排序

| 优先级 | 问题 | 影响 | 难度 |
|--------|------|------|------|
| P0 | ConnectOptions/SyncOptions K8s 客户端重复 | 维护负担 | 低 |
| P0 | Envoy 配置无 schema 版本号 | 升级兼容性 | 中 |
| P1 | daemon action 无统一 forwarding 抽象 | 代码重复/bug | 高 |
| P1 | Cleanup 双路径 (isDataPlane) | 可读性 | 中 |
| P1 | util/ 拆包 | 架构清晰度 | 高 |
| P2 | DefaultRouteHub 全局单例 | 可测试性 | 低 |
| P2 | Rule 无 OwnerID | 数据完整性 | 中 |
| P2 | PortMap 字符串编码 | 可维护性 | 低 |
| P2 | config/ init() panic 风险 | 健壮性 | 低 |
| P3 | cmd/ 导入过多内部包 | 分层违规 | 高 |
| P3 | ParseForwarder 忽略协议 | 抽象泄漏 | 低 |
| P3 | IP key 热路径分配 | 性能 | 低 |
