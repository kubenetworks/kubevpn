# Rule OwnerID 设计文档

## 1. 问题陈述

### 1.1 现状

KubeVPN 支持多用户同时代理同一个 K8s 工作负载。每个用户的代理规则存储在 `controlplane.Rule` 结构体中，以 YAML 序列化后保存在集群 ConfigMap (`kubevpn-traffic-manager`) 的 `ENVOY_CONFIG` 键中。

数据模型：

```
ConfigMap
  └── ENVOY_CONFIG (YAML)
       └── []Virtual
            ├── UID: "deployments.apps.web"
            ├── Namespace: "default"
            └── Rules:
                 ├── Rule[0]: {Headers: {version: v1}, LocalTunIPv4: "198.18.0.5", ...}
                 └── Rule[1]: {Headers: {version: v2}, LocalTunIPv4: "198.18.0.9", ...}
```

### 1.2 所有权判断方式（改进前）

判断一条 Rule "属于谁" 依赖两种隐式推断，取决于代理模式：

| 模式 | 判断逻辑 | 代码位置 |
|------|---------|---------|
| 标准模式 (VPN/Mesh) | `rule.LocalTunIPv4 == 当前用户TUN IP` | `handler/leave.go:63` |
| Fargate/Service 模式 | `reflect.DeepEqual(rule.Headers, 用户Headers)` | `handler/leave.go:61` |

### 1.3 问题

**问题 1：IP 重分配导致误匹配**

```
时间线:
T1: 用户 A 连接，DHCP 分配 198.18.0.5，创建 Rule{LocalTunIPv4: "198.18.0.5"}
T2: 用户 A 异常断开（未清理 Rule）
T3: 用户 B 连接，DHCP 分配 198.18.0.5（复用了相同 IP）
T4: 用户 B 执行 leave → 匹配到用户 A 的遗留 Rule（因为 IP 相同）→ 误删
```

根因：DHCP 基于 ConfigMap 的 bitmap 分配 IP，已释放的 IP 会被立即复用。Rule 的 IP 字段没有与具体连接会话绑定。

**问题 2：孤儿规则无法溯源**

当用户异常断开（进程被 kill、网络中断）时，其 Rule 残留在 ConfigMap 中。管理员无法知道：
- 这条 Rule 是谁创建的
- 创建于什么时间
- 对应的连接是否仍然活跃

**问题 3：多连接场景的所有权模糊**

一个用户可以同时连接多个集群，每个连接有不同的 TUN IP。如果两个连接代理了同一个工作负载的不同版本，现有机制无法区分"同一用户的不同连接"和"不同用户"。

## 2. 设计方案

### 2.1 新增 OwnerID 字段

在 `Rule` 结构体中添加显式的所有者标识字段：

```go
type Rule struct {
    Headers      map[string]string
    LocalTunIPv4 string
    LocalTunIPv6 string
    OwnerID      string `yaml:"ownerID,omitempty" json:"ownerID,omitempty"`
    PortMap      map[int32]string
}
```

### 2.2 OwnerID 的值

使用 **UUID（前 12 位）** 作为 OwnerID 的值，在 `DoConnect` 中每次连接生成一个唯一标识：

```go
// handler/connect.go — DoConnect 入口
c.ownerID = uuid.New().String()[:12]  // e.g., "a1b2c3d4-e5f"
```

**为什么用 UUID 而非 IP？**

| 方案 | 问题 |
|------|------|
| LocalTunIPv4 | IP 可被 DHCP 重分配给其他用户，导致误匹配 |
| ConnectionID (namespace UID) | namespace 级别标识，同 namespace 多用户无法区分 |
| UUID | 全局唯一，不受 IP 重分配影响 |

**为什么不用完整 UUID？**

完整 UUID 36 字符，存储在 ConfigMap YAML 中会增加体积。12 字符的前缀（~72 bit 熵）在实际并发用户数（通常 < 100）下碰撞概率可忽略。

### 2.3 OwnerID 在双 Daemon 架构中的位置

**OwnerID 只存在于 User Daemon（控制面）**，不在 Root Daemon（数据面）。

详见 [docs/12-dual-daemon-architecture.md](12-dual-daemon-architecture.md)。

- 生成：`daemon/action/connect.go` → `redirectConnectToSudoDaemon()` 构造 ConnectOptions 时
- 使用：`handler/connect.go` → `CreateRemoteInboundPod()` → `inject.NewInjector(OwnerID)`
- 持久化：`OffloadToConfig()` 序列化到 `~/.kubevpn/daemon/db`
- 恢复：`LoadFromConfig()` 反序列化，`DoConnect` 中 `if OwnerID == ""` 不会覆盖

Root Daemon 的 `DoConnect()` **不生成 OwnerID**——它只做 TUN/路由/DNS，不操作 Envoy 配置。

### 2.4 四种写入场景

`addVirtualRule` 函数处理 4 种场景，每种都正确设置 OwnerID：

```
Case 1: 全新工作负载（index < 0）
  → 创建 Virtual + Rule，设置 OwnerID = uuid

Case 2: 同一用户更新（IP 匹配，非 Fargate）
  → 合并 Headers/PortMap，如果 OwnerID 为空则补填（向后兼容）

Case 3: 所有权转移（Headers 匹配）
  → 更新 LocalTunIPv4/v6 和 OwnerID = 新用户的 uuid

Case 4: 新用户加入（不同 Headers，不同 IP）
  → 追加新 Rule，设置 OwnerID = localTunIPv4
```

### 2.4 向后兼容

| 场景 | 行为 |
|------|------|
| 新版本读取旧 ConfigMap | OwnerID 反序列化为 `""`（零值），不影响现有逻辑 |
| 旧版本读取新 ConfigMap | OwnerID 字段被忽略（Go YAML 解析器忽略未知字段） |
| 新版本更新旧 Rule | Case 2 中 `if OwnerID == ""` 自动补填 |
| 混合版本并发 | 安全——OwnerID 是补充字段，不参与现有匹配 |

`omitempty` 标签确保空 OwnerID 不会写入 YAML，减少 ConfigMap 体积。

## 3. ConfigMap YAML 示例

### 升级前

```yaml
- uid: deployments.apps.web
  namespace: default
  ports:
  - containerPort: 8080
    protocol: TCP
  rules:
  - headers:
      version: v1
    localtunipv4: "198.18.0.5"
    localtunipv6: "2001:2::5"
    portmap:
      8080: "9090"
```

### 升级后

```yaml
- schemaVersion: 1
  uid: deployments.apps.web
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
      8080: "9090"
```

注意 `ownerID` 是 UUID 前 12 位，与 `localtunipv4` 无关。即使 IP 被重分配给其他用户，OwnerID 仍然唯一标识原始创建者。

## 4. 未来演进计划

### Phase 1（已完成）：UUID 化 OwnerID

- OwnerID 使用 UUID 前 12 位，在 `DoConnect` 中生成
- 不改变现有的 IP/Headers 匹配逻辑
- 所有新创建和更新的 Rule 都带上 OwnerID
- 旧 Rule 在被更新时自动补填

### Phase 2（下一步）：OwnerID 参与匹配

```go
// leave.go 中的匹配逻辑演进
func isMyRule(rule *controlplane.Rule, myOwnerID, myIPv4 string) bool {
    // 优先用 OwnerID 匹配（如果双方都有）
    if rule.OwnerID != "" && myOwnerID != "" {
        return rule.OwnerID == myOwnerID
    }
    // 降级到 IP 匹配（兼容旧规则）
    return rule.LocalTunIPv4 == myIPv4
}
```

### Phase 3：孤儿规则自动清理

```go
// 定期扫描 ConfigMap，清理不活跃连接的 Rule
func (c *ConnectOptions) GarbageCollectRules(ctx context.Context) {
    virtuals := loadVirtuals(ctx)
    for _, v := range virtuals {
        for _, rule := range v.Rules {
            if rule.OwnerID != "" && !isConnectionActive(rule.OwnerID) {
                removeRule(ctx, v, rule)
            }
        }
    }
}
```

## 5. 关联修改

| 文件 | 修改 |
|------|------|
| `pkg/controlplane/cache.go` | 新增 `OwnerID` 字段到 `Rule` 结构体 |
| `pkg/inject/envoy.go` | 在 `addVirtualRule` 的 4 个 case 中设置 OwnerID |
| `pkg/inject/envoyrule_test.go` | 更新测试期望值包含 OwnerID |

## 6. 风险评估

| 风险 | 等级 | 缓解措施 |
|------|------|---------|
| 旧版本客户端不理解 OwnerID | 低 | `omitempty` + Go YAML 忽略未知字段 |
| OwnerID 与 LocalTunIPv4 不一致 | 低 | 当前阶段两者值相同 |
| ConfigMap 体积增大 | 极低 | 每条 Rule 增加 ~30 字节 |
| Phase 2 匹配逻辑变更 | 中 | 需要全面测试，建议在独立 PR 中实施 |
