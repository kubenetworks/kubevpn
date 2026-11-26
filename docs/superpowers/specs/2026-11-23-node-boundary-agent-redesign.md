# Node-Boundary Agent 架构重设计

- **日期:** 2026-11-23
- **状态:** approved（待实施）
- **范围:** 重新设计 node-agent 接管层；sidecar 与 AI agent/MCP 不在本次变更范围
- **前序文档:** `docs/55-node-hosted-agent.md`、`docs/superpowers/specs/2026-11-22-nodeagent-dataplane-convergence-design.md`

## 1. 背景与结论

从 commit `d0cb7e4623140b2dc7f41ee963189da5abb5949e` 起，目标只是把默认 attach mode 从 sidecar 改为 node-agent。默认切换 `8ddc55eba` 后又产生约 40 个修复提交、约 6000 行变更。真集群 CI 暴露了 attach-mode 覆盖、Job churn、同 netns 双 envoy、agent/user 地址池身份、路由自注册黑洞等问题；修补过程中也出现过 exec probe 和客户端 destination guard 的错误设计。

结论：**node-agent 的非侵入目标成立，但“每 pod 一个特权 Job + 每 pod 一条隧道/租约 + 进入 pod netns”的形态不合理。** 它不是默认值切换，而是一次数据面架构迁移。继续给 per-pod Job 体系加守卫不再作为推进方向。

## 2. 硬约束与非目标

### 硬约束

1. 不修改 workload spec，不滚动重启业务 pod。
2. **不使用 `hostNetwork`**。
3. 接受节点级 DaemonSet；hostPID、只读宿主机 `/proc`、必要 capabilities 可使用，但不扩大为宿主机全权控制。
4. sidecar 保留为兼容/降级路径，不依赖 sidecar 掩盖 node-agent 缺陷。
5. 现有 OwnerID/header 路由语义不变；现有 `proxy.attachMode=node-agent` 对外语义不变。

### 非目标

- 不做每节点共享单 envoy。
- 不做 eBPF/CNI 插件。
- 不删除 sidecar。
- 不改变 MCP/AI agent 功能。

## 3. 总体架构：无 hostNetwork 的节点边界接管

每节点运行一个 DaemonSet agent：

- `hostNetwork=false`，agent 使用自己的 Pod IP。
- `hostPID=true`，宿主机 `/proc` 只读挂载；capabilities 以 `SYS_ADMIN` + `NET_ADMIN` 为目标，PoC 验证最小集合，不默认 `privileged=true`。
- **每节点一条到 manager 的隧道、一个 TUN IP、一个 lease**，身份为 `OwnerID=nodeName, IsAgent=true`。
- 不进入业务 pod netns，不创建 per-pod Job，不创建 per-pod agent 租约。
- agent 只在配置规则时进入宿主机 netns，按目标 pod IP 安装 DNAT，目标是 agent 自己的 Pod IP 和 workload 端口。

### 数据路径

```text
进入目标 pod 的流量
  → 宿主机 PREROUTING 链首（KUBE-SERVICES 前）：精确匹配 dst=<podIP|ServiceClusterIP> 与原始端口，且排除 src=<agentPodIP>
  → DNAT 到 <agentPodIP>:<agentListenerPort>
  → agent pod 内该 workload 的 envoy
       ├─ 命中 header 规则 → 用户 TUN IP（经本节点唯一隧道）
       └─ 未命中           → 原始 podIP:port（由 agent pod 直接转发）
  → 回程经 agent pod 的 TUN/路由返回
```

- TCP 使用上述 DNAT + 每 pod/原始端口独立直连 listener；manager 为每个 listener 配置显式 `<podIP>:<containerPort>` 回源 cluster。宿主机与 agent 分属不同 netns，不能依赖 `ORIGINAL_DST` 或 sidecar capture listener 还原原始目的地址。
- Service ClusterIP 是独立 origin：kube-proxy/IPVS 可在宿主机 `INPUT` 阶段才转发 Service，因此只匹配 podIP 会漏掉 Service 流量。所有接管规则必须以 `-I PREROUTING 1` 插入链首，不能追加在 KUBE-SERVICES 之后。Service attachment 按节点发布，`originIP=ClusterIP`、`originPort=ServicePort`，未命中时回源 ClusterIP，由 kube-proxy 继续负载均衡；不能用固定 podIP 替代。
- **必须排除 `src=<agentPodIP>`**：否则 agent 向原始 podIP 做 origin-forwarding 时会被同一条宿主机 DNAT 再次捕获，形成回环并返回 503（PoC 实测）。
- UDP 使用 agent 的 TUN 数据面与现有 UDP 配置处理，**必须在 PoC 中单独验证，不能由 TCP 通过推断 UDP 可用**。
- 已有 kubevpn sidecar 的 pod 跳过接管：一个 pod netns 只允许一个 kubevpn 接管者。
- manager 自身 ClusterIP 不作为目标 pod，不会被这些精确 podIP 规则捕获。

## 4. 控制面与配置模型

### 4.1 Virtual

在现有 `controlplane.Virtual` 增加：

```yaml
agentListenerPort: 20000
agentOrigins:
- podUID: 8e02efad-8dd9-452d-bdcc-6e76b5c5480f
  podIP: 10.244.0.42
  ports:
  - containerPort: 9080
    protocol: TCP
    agentListenerPort: 20001
```

- 端口范围：`20000-24999`；每 Virtual/workload 的唯一 `agentListenerPort` 作为 Envoy admin/base-id 基准，由 manager 分配并持久化。
- `agentOrigins` 是 manager 按“节点 + workload”派生的配置；每个 pod/容器端口分配独立且稳定的直连 listener，回源 endpoint 显式指向对应 podIP。
- sidecar 继续使用 `15006` 与 `ORIGINAL_DST`；node-boundary 不使用 sidecar capture listener。
- envoy admin 端口固定为 `agentListenerPort + 5000`（范围 `25000-29999`），避免多个 envoy 共享 `9003`，且无需第二次独立分配。
- `Namespace/UID/Selector/Rules/OwnerID` 保持现有语义；派生 Virtual 的 UID 为 `<workloadUID>.nodes.<nodeName>`。派生项仅供数据面使用，status 与 leave-all 必须过滤它们，只展示/解析 base workload。

### 4.2 NodeAttachment

manager 的中央 reconciler 只发布期望状态，不创建或对齐任何 Job：

```yaml
podUID: 8e02efad-8dd9-452d-bdcc-6e76b5c5480f
nodeName: minikube
podIP: 10.244.0.42
workloadUID: deployments.apps.reviews
nodeID: deployments.apps.reviews.nodes.minikube
agentListenerPort: 20000
ports:
- containerPort: 9080
  protocol: TCP
  agentListenerPort: 20001
```

记录写入 traffic-manager ConfigMap 的新 key `NODE_ATTACHMENTS`。

- manager 根据 Virtual.Selector 找到目标 pod，生成/更新/删除附件。
- 同一节点、同一 workload 的多个 pod 共用一个 Envoy 和派生 nodeID；每个 pod/容器端口使用独立 listener port。
- `AGENT_PORT_ALLOCATIONS` 持久化 workload/origin 的分配与 tombstone；leave 只移除附件，不立即把端口分给另一 workload，re-proxy 恢复原端口，避免与尚未退出的 Envoy 冲突。
- 中央 reconciler 是配置事实源，不再承担 agent pod 健康检查和 Job 重建。

### 4.3 节点 agent

DaemonSet agent 仅执行本地动作：

1. 通过 Downward API 获得 `nodeName`、`podIP`。
2. watch `NODE_ATTACHMENTS`，筛选本节点记录。
3. 在宿主机 netns 收敛精确 podIP DNAT 规则。
4. 按 workload 启停 envoy，xDS nodeID 使用 `GenEnvoyUID(namespace, <workloadUID>.nodes.<nodeName>)`，订阅 manager 派生的本节点直连配置。
5. 维持本节点唯一隧道和 lease。

agent 不需要 batch/jobs、`pods/exec` 或全集群 pod list/watch 权限。

## 5. 生命周期与故障语义

- **目标 pod 创建/重建：** manager 更新附件；agent 按新 podIP 替换规则。无 per-pod 隧道或租约迁移。
- **leave：** manager 删除附件；agent 删除对应规则；该节点最后一个附件消失时停止对应 workload envoy。
- **sidecar 共存：** manager 不发布附件，旧附件若存在则删除。
- **agent 重启：** 启动时先清理本机带 `kubevpn-nodeagent` comment 的旧规则，再按当前附件重建。agent Pod IP 变化时旧规则不得残留。允许短暂中断，验收要求 60 秒内恢复。
- **manager 重启：** ConfigMap 与附件不变，agent 保留配置并继续转发；隧道重连和 lease 续租使用现有退避机制。
- **规则冲突：** agent 启动与每次附件变更都采用收敛语义，不依赖旧进程完成清理。
- **无法判断：** 不批量删除无法确认归属的规则，不用 restart loop 代替诊断。

## 6. 迁移与删除范围

### 阶段 0：最小真集群 PoC

先证明数据路径，不先大改控制面。单个 DaemonSet + 静态附件 + 一个 workload 验证：

- ServiceIP/PodIP、命中 header 到本地、未命中到远端、响应回程。
- UDP 独立验证。
- 同节点第二个 workload 无端口冲突。
- 业务 pod UID 与 Deployment generation 不变。
- `hostNetwork=false`。

PoC 失败则停止此路线并回退默认 sidecar，不以大规模实现弥补未证实的前提。

### 阶段 1：替换接管层

- manager 实现 Virtual 端口分配与 NodeAttachment 发布。
- DaemonSet agent 实现附件消费、宿主机规则收敛、envoy 启停和单节点隧道。
- **同一变更中禁用旧 per-pod Job reconciler**，禁止双轨接管。
- sidecar 路径不变。

### 阶段 2：验证后清理

删除或停用：per-pod Job 创建/对齐、Job RBAC、terminating-job 等待、per-pod agent 健康检查及相关遗留测试。隐藏旧调试命令可以保留但不再调用，避免修改现有 `cmd/` 行为。

双池 schema 暂保留以兼容已有配置和节点级 agent lease；每节点最多一条 agent lease，不再按 pod 分配。

## 7. 测试与合并门禁

### 集成测试（fake clientset）

- Virtual 端口分配唯一且持久，删除后不复用仍在使用的端口。
- NodeAttachment 随 Virtual/pod 创建、重建、leave 正确发布。
- sidecar-carrying pod 不发布附件。
- agent 对本节点附件收敛、忽略其他节点、清理陈旧规则。
- manager/agent 重启不造成重复附件或重复 envoy。

### 真集群验收

- 现有 `TestFunctions`、`TestSSHFunctions` 全绿：full-proxy、mesh、portmap、sync、run、central、SSH。
- 新增生命周期场景：agent 删除重启、manager 重启、目标 pod 重建、leave→re-proxy。
- 同节点多 workload、同 workload 多 pod、sidecar 共存，无端口冲突与 reconciler churn。
- 权限断言：`hostNetwork=false`、无 per-pod Job、无 `pods/exec`。
- 非侵入断言：attach 前后 workload generation 与 pod UID 不变。
- Linux CI 超过 40 分钟按失败处理，立即输出 NodeAttachment、宿主机规则和 agent 日志。

### 合并门禁

若新架构未在真集群矩阵全绿，PR 合并前必须将默认模式回退为 sidecar，node-boundary agent 仅保留为 opt-in experimental；不得带失败 CI 将 node-agent 作为默认模式合并。

## 8. 主要剩余风险

1. **CNI 差异：** 宿主机 PREROUTING 对 podIP 的可见性与规则顺序需在 minikube docker driver 和 ACK 真集群分别验证。
2. **agent Pod IP 变化：** 重启时必须先清旧规则再重建，防止 DNAT 指向旧 IP；由生命周期测试覆盖。
3. **UDP 语义：** 不能从 TCP 成功推导 UDP 成功，PoC 单独门禁。
4. **端口分配兼容性：** 老配置没有 `agentListenerPort` 时由 manager 分配并持久化，不能每次重启重新随机分配。

这些风险均有前置验证或回退条件，不再以“先合并默认模式再修 CI”的方式处理。
