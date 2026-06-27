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
                 ├── Rule[0]: {Headers: {version: v1}, LocalTunIPv4: "198.18.0.5", OwnerID: "a1b2c3d4e5f6", ...}
                 └── Rule[1]: {Headers: {version: v2}, LocalTunIPv4: "198.18.0.9", OwnerID: "f6e5d4c3b2a1", ...}
```

### 1.2 所有权判断方式

所有模式（标准/Fargate）统一使用 `rule.OwnerID == connect.OwnerID` 进行匹配：

| 场景 | 判断逻辑 | 代码位置 |
|------|---------|---------|
| Leave (unpatch) | `ownerID == rule.OwnerID` | `inject/envoy.go:removeEnvoyConfig` |
| Status (CurrentDevice) | `rule.OwnerID == connect.OwnerID` | `daemon/action/status.go` |

之前标准模式用 IP 匹配、Fargate 模式用 Headers 匹配的双分支逻辑已被统一。

## 2. 设计方案

### 2.1 OwnerID 字段

`Rule` 结构体中的显式所有者标识字段：

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

使用 **UUID（前 12 位）** 作为 OwnerID 的值，在 User Daemon 的 `redirectConnectToSudoDaemon` 中每次连接生成：

```go
// daemon/action/connect_elevate.go
OwnerID: uuid.New().String()[:12],  // e.g., "a1b2c3d4e5f6"
```

通过 `ConnectRequest.OwnerID` proto 字段传递给 Root Daemon。

**为什么用 UUID 而非其他标识？**

| 方案 | 问题 |
|------|------|
| LocalTunIPv4 | IP 可被 DHCP 重分配给其他用户，导致误匹配 |
| ConnectionID (namespace UID) | namespace 级别标识，同 namespace 多用户无法区分 |
| UUID | 全局唯一，不受 IP 重分配影响 |

### 2.3 OwnerID 在双 Daemon 架构中的传递

```
User Daemon:
  OwnerID = uuid.New().String()[:12]         ← 生成
  → req.OwnerID = connect.OwnerID            ← 写入 ConnectRequest
  → CreateRemoteInboundPod(..., ownerID)     ← 注入 envoy rule
  → LeaveResource(..., ownerID)              ← 移除 envoy rule

Root Daemon:
  connect.OwnerID = req.OwnerID              ← 从请求接收
  → NetworkManager.rentIP(OwnerID)           ← 用于 TunConfigService IP 分配
```

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
  → 追加新 Rule，设置 OwnerID = uuid
```

### 2.5 删除场景

`removeEnvoyConfig` 直接用 `ownerID` 匹配删除：

```go
for i := 0; i < len(virtual.Rules); i++ {
    if ownerID == virtual.Rules[i].OwnerID {
        found = true
        virtual.Rules = append(virtual.Rules[:i], virtual.Rules[i+1:]...)
        i--
    }
}
```

不再需要 `isFargateMode` 分支——OwnerID 匹配对所有模式都有效。

### 2.6 向后兼容

| 场景 | 行为 |
|------|------|
| 新版本读取旧 ConfigMap | OwnerID 反序列化为 `""`（零值），不影响现有逻辑 |
| 旧版本读取新 ConfigMap | OwnerID 字段被忽略（Go YAML 解析器忽略未知字段） |
| 新版本更新旧 Rule | Case 2 中 `if OwnerID == ""` 自动补填 |
| 新版本 leave 旧 Rule（OwnerID 为空）| 不会匹配到（`uuid != ""`），需先 proxy 一次补填 OwnerID |

`omitempty` 标签确保空 OwnerID 不会写入 YAML，减少 ConfigMap 体积。

## 3. ConfigMap YAML 示例

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

`ownerID` 是 UUID 前 12 位，与 `localtunipv4` 无关。即使 IP 被重分配给其他用户，OwnerID 仍然唯一标识原始创建者。

## 4. 关联文件

| 文件 | 作用 |
|------|------|
| `pkg/controlplane/cache.go` | `Rule.OwnerID` 字段定义 |
| `pkg/inject/envoy.go` | `addVirtualRule` 写入 OwnerID，`removeEnvoyConfig` 用 OwnerID 匹配删除 |
| `pkg/inject/mesh.go` | `UnpatchContainer` 接收 ownerID 参数 |
| `pkg/handler/proxy_manager.go` | `Leave/LeaveAll` 传 ownerID 给 `UnpatchContainer` |
| `pkg/daemon/action/status.go` | `CurrentDevice` 用 `rule.OwnerID == connect.OwnerID` 判断 |

## 5. 风险评估

| 风险 | 等级 | 缓解措施 |
|------|------|---------|
| 旧版本客户端不理解 OwnerID | 低 | `omitempty` + Go YAML 忽略未知字段 |
| 旧 Rule 无 OwnerID 导致无法 leave | 低 | 执行 proxy 时 Case 2/3 自动补填 OwnerID |
| ConfigMap 体积增大 | 极低 | 每条 Rule 增加 ~30 字节 |
