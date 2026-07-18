# Stable / Fixed TUN IP Allocation Design

## 1. 背景与问题

KubeVPN 为每个 VPN 连接分配一个 TUN 设备 IP（地址池 `198.18.0.0/16`，见
`docs/03-dhcp-ip-allocation.md`）。分配由集群内 traffic-manager 的
`TunConfigServer` 负责，租约按 **OwnerID** 记录在 ConfigMap 的 `TUN_ALLOCS`
键中，底层位图分配走 `cilium/ipam`。

**问题：同一客户端每次重连拿到的 TUN IP 是不稳定的。**

根因：OwnerID 在每次 connect 时随机生成。

```go
// pkg/daemon/action/connect_elevate.go:31（用户守护进程内）
ownerID := req.OwnerID
if ownerID == "" {
    ownerID = uuid.New().String()[:12] // ← 每次连接都是新随机值
}
```

由此带来两种不稳定：

| 场景 | 现状 | 后果 |
|------|------|------|
| 租期内（< 5min）重连 | 旧 OwnerID 的租约仍持有旧 IP，新 OwnerID 分到 **另一个** IP | IP 变化；旧 IP 被白白占用到过期 |
| 租约回收后（> 5min）重连 | 连续位图从 0 扫描，给"下一个空闲"，只是**碰巧**可能相同 | IP 不可预期 |

## 2. 目标与范围

- **目标**：尽最大努力让同一客户端在重连后拿回**同一个** TUN IP。
- **粒度**：身份按 **(机器 + 操作系统用户)**，即同一台机器、同一 OS 用户稳定唯一。
- **性质**：尽力而为（best-effort），**不是**硬性永久预留——离线足够久且该 IP 被他人占用时，会干净回退到新 IP。
- **不在范围内**：
  - 永久预留 / 静态分配（离线也不回收、不被他人占用）。
  - 按"人"跨机器固定（基于 kubeconfig 用户/证书）。

设计分两层，叠加生效：

- **Part A 稳定 OwnerID**：解决租期内重连（高频场景），并消除"每次重连浪费一个 IP"。
- **Part B preferred-IP 提示**：解决租约回收后（长时间断开）的重连。

## 3. 现状分析（可复用的基础设施）

- 租约层 `pkg/controlplane/tun_config.go`：`GetTunIP` 已支持
  - 同一 OwnerID 续租返回同一 IP（`s.allocs[ownerID]` 命中即返回）；
  - `ExcludeIPs` 冲突时重分配。
- 位图层 `pkg/dhcp/dhcp.go`：`RentIPExcluding` 走 `AllocateNext`（顺序分配）。
- 底层 `cilium/ipam` 同时提供**指定 IP 分配**与顺序分配：
  ```go
  // vendor/github.com/cilium/ipam/service/ipallocator/allocator.go
  func (r *Range) Allocate(ip net.IP) error   // 指定 IP，被占用则报错
  func (r *Range) AllocateNext() (net.IP, error)
  ```
- 客户端分配入口 `pkg/handler/network.go` 的 `rentIP`（运行在 **root 守护进程**），
  通过 `TunIPRequest{OwnerID, Namespace, ExcludeIPs}` 调 `GetTunIP`。
- 持久化目录 `~/.kubevpn`（`pkg/config/const.go` 的 `homePath`），项目已依赖
  `github.com/google/uuid`。

## 4. 设计方案

### 4.1 Part A：稳定 OwnerID（持久化 UUID，按机器+用户）

新增 `config.GetClientID()`：

```
首次调用：生成 UUID → 写入 ~/.kubevpn/client_id（文件不存在时）
后续调用：读取并复用
返回值：取前 12 位，沿用现有 OwnerID 格式
```

`connect_elevate.go` 中把随机生成改为：

```go
if ownerID == "" {
    ownerID = config.GetClientID() // 稳定、可持久化
}
```

这段逻辑运行在**用户守护进程**，`~/.kubevpn` 即调用者（OS 用户）的家目录，
因此 OwnerID 自动满足：

- 同一机器、同一 OS 用户 → 稳定唯一，且跨重连、跨守护进程重启不变；
- 不同 OS 用户 → 不同家目录 → 不同 OwnerID。

**仅 Part A 的效果**：租期内（5min）重连命中 `s.allocs[ownerID]`，直接返回同一
IP；不再每次重连消耗一个新 IP。

### 4.2 Part B：服务端"上次 IP"记忆（长时间断开后的粘性）

租约被回收后，`TUN_ALLOCS` 中该 OwnerID 的记录已删除。要在重连时拿回旧 IP，需要
有人记得"这个 OwnerID 上次用的是哪个 IP"。

**实现选型说明**：原设计是"客户端把上次 IP 作为 `PreferredIPs` 经 gRPC 传上来"，
需要给 `TunIPRequest` 新增字段并 `make gen` 重新生成 protobuf。由于构建环境无
`protoc`、且仓库约定不手改 `*.pb.go`，改为**等价的服务端实现**：既然 Part A 已让
OwnerID 稳定，traffic-manager 自己按 OwnerID 记住"上次分配的 IP"即可，无需新增协议
字段，也无需客户端持久化。跨集群本地唯一性仍由客户端已经发送的 `ExcludeIPs`
（Fix 3）保证。

#### 4.2.1 服务端记忆 lastIPs

`pkg/controlplane/tun_config.go` 的 `TunConfigServer` 增加内存映射：

```go
lastIPs map[string]lastIPRecord // ownerID → 上次持有的 {v4, v6}
```

- **写入时机**：`reapExpiredLeases` 回收某 OwnerID 的租约、`delete(allocs, ownerID)`
  之前，先 `lastIPs[ownerID] = {alloc.IPv4, alloc.IPv6}`。
- **特性**：仅内存（traffic-manager 重启后丢失，见 §7）；按 OwnerID 作 key，Part A
  已保证其按用户稳定唯一，因此条数受真实客户端数量天然约束（不再像随机 UUID 那样
  无界增长）。

#### 4.2.2 位图层支持指定 IP

`pkg/dhcp/dhcp.go`：把 `RentIPExcluding` 重构为共享 `makeShouldSkip` /
`allocateOne` 的内部 `rentIP`，并新增：

```go
// 先尝试 ipallocator.Allocate(指定IP)，被占用/不可用/被 skip 则回退 AllocateNext。
func (m *Manager) RentIPPreferring(ctx, prefV4, prefV6 net.IP, exclude []net.IP) (*net.IPNet, *net.IPNet, error)
```

#### 4.2.3 GetTunIP 优先复用

`pkg/controlplane/tun_config.go` 的 `GetTunIP`，**仅在"新分配"路径**（该 OwnerID
当前无活跃租约）改为调用 `allocateForOwner`：

```
若 lastIPs[ownerID] 存在 且 该 IP 不在 ExcludeIPs 中
        → RentIPPreferring(lastIP, exclude)   // 优先复用旧 IP
否则    → RentIPExcluding(exclude)             // 顺序分配（维持现状）
```

"已有租约续租"和"ExcludeIPs 冲突重分配"两条路径不变。Fix 1/2 的不变量（单写者、
持久化失败回滚、孤儿 bit 清扫）全部保留。

### 4.3 数据流

```
重连（同一客户端）
  用户守护进程: OwnerID = GetClientID()  ──(ConnectRequest.OwnerID)──▶ root 守护进程
  root 守护进程 rentIP: GetTunIP{OwnerID, ExcludeIPs} ─────▶ traffic-manager
        ├─ 有活跃租约       → 返回原 IP（Part A）
        └─ 无活跃租约 allocateForOwner：
              lastIPs[OwnerID] 存在且不在 ExcludeIPs → RentIPPreferring(旧IP)
                    旧 IP 空闲 → 复用（Part B）
                    否则       → AllocateNext 回退
              否则                                  → RentIPExcluding（顺序）
```

## 5. 多集群 / 多用户 / 多 sidecar 正确性

| 维度 | 分析 | 结论 |
|------|------|------|
| **同集群多用户** | OwnerID 按 OS 用户家目录不同而不同；`lastIPs` 复用仅在该 IP **空闲**时命中，被占则 `Allocate` 失败回退 | 不会两人同 IP |
| **单客户端多集群** | 每集群独立 ConfigMap/allocs，OwnerID 相同无妨；但本机要求各集群 TUN IP **不重叠** | 见下一行 |
| **跨集群本地唯一性** | 客户端发送的 `ExcludeIPs` 含兄弟连接已持有的 TUN IP（Fix 3，`buildExcludeIPs`）；服务端复用 `lastIPs` 前会检查 `ExcludeIPs`，命中则跳过 | 跨集群本地仍唯一，复用让位 |
| **多 sidecar** | sidecar 的 OwnerID 是 podName，与客户端 UUID 是不相交的 key 空间 | 无冲突 |
| **同集群单客户端多连接** | 用户守护进程按 ConnectionID（manager namespace UID）去重，一集群至多一条连接 | 同一 OwnerID 不会在一个集群出现两条租约 |

## 6. 与现有机制的关系

- **租约 / reaper**（`docs/03`）：不变。`lastIPs` 只影响"无活跃租约时如何选 IP"，
  不改变租期与回收。
- **滚动升级安全 Fix 1**、**位图↔allocs 防泄漏 Fix 2**、**envoy 规则 IP 同步
  Fix 4**：均不受影响，继续生效。
- **跨集群本地 IP 排除 Fix 3**：`lastIPs` 复用受客户端发送的 `ExcludeIPs` 约束，
  本机唯一性优先级高于"复用同一 IP"。
- **休眠唤醒 `docs/28`**：稳定 OwnerID + `lastIPs` 提升唤醒后拿回同一 IP 的概率；
  但 VPN-only sidecar 的 iptables DNAT 硬编码热更新仍是独立遗留项，不在本设计内。

## 7. 失败与回退（边界场景）

| 场景 | 行为 |
|------|------|
| `~/.kubevpn/client_id` 不可写 | 回退为本次随机 UUID（退化为现状，不影响功能） |
| 旧 IP 已被他人占用 | `Allocate` 失败 → `AllocateNext` 回退到新 IP |
| 旧 IP 与本机其它集群冲突 | 在 `ExcludeIPs` 中 → 跳过复用 → 回退 |
| traffic-manager 重启 | `lastIPs` 为内存态会丢失 → 退化为 Part A（活跃租约仍稳定；已回收的超期连接首次重连可能换 IP，之后再次稳定） |

## 8. 接口与文件清单

| 文件 | 变更 |
|------|------|
| `pkg/config/clientid.go`（新增） | `GetClientID()`（持久化稳定客户端 UUID） |
| `pkg/config/const.go` | 复用 `homePath` |
| `pkg/daemon/action/connect_elevate.go` | 用 `GetClientID()` 取代随机 UUID |
| `pkg/dhcp/dhcp.go` | 重构 `rentIP`/`makeShouldSkip`/`allocateOne`，新增 `RentIPPreferring` |
| `pkg/controlplane/tun_config.go` | `lastIPs` 记忆 + `allocateForOwner`，`GetTunIP` 新分配路径优先复用 |

> 注：原计划的 `daemon.proto` 新字段与 `pkg/handler/network.go` 客户端改动因
> 无 `protoc` 环境而改为服务端 `lastIPs` 方案（见 §4.2），不再涉及。

## 9. 测试与验证

集成测试（`fake.NewSimpleClientset` + `controlplane` 的 `newTestServer`）：

- 被回收后同一 OwnerID 重连 → 复用旧 IP，即便存在更低号的空闲 IP
  （`TestGetTunIP_StickyReconnectPrefersRememberedIP`）。
- 旧 IP 已被他人占用 → 回退到另一个空闲 IP，不串号
  （`TestGetTunIP_StickyFallsBackWhenRememberedIPTaken`）。
- 旧 IP 出现在 `ExcludeIPs` → 让位回退（`TestGetTunIP_StickyYieldsToExcludeIPs`，Fix 3 优先）。
- 同一 OwnerID 续租 → 同 IP（已有 `TestGetTunIP_RenewReturnsSameIP` 覆盖）。
- `config.GetClientID()` 多次调用稳定且持久化（`TestGetClientID_StableAndPersisted`）。

通用：`go build ./...`（linux/darwin/windows）、`go vet ./pkg/...`、
`go test ./pkg/...`（已知预存在失败：`TestPing` 环境限制、偶发
`TestTUN_FullDataPath_HTTPRequest`）。

端到端（`/data/.kube/config`，需切到远端 context）：connect 记录 IP → 断开 →
5min 内重连（Part A 同 IP）与 >5min 重连（Part B 若空闲则同 IP）；两集群并连验证
本机 IP 仍各不相同。
