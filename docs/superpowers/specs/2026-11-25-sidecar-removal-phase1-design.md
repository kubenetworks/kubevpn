# Sidecar Removal Phase 1 — 普通 Proxy 脱离 Sidecar

- **日期:** 2026-11-25
- **状态:** approved（待实施）
- **上游架构:** `docs/superpowers/specs/2026-11-23-node-boundary-agent-redesign.md`
- **PoC 证据:** `docs/superpowers/poc/2026-11-23-node-boundary-result.md`

## 1. 目标与范围

最终目标是彻底移除 sidecar，但采用分阶段迁移，不做一次性全量替换。

**本阶段只处理普通工作负载的 proxy/connect：**

- 非 Service 工作负载统一使用 node-boundary agent。
- 普通 proxy 的代码路径不再创建或依赖 sidecar。
- 不再使用旧的 per-pod Job 架构。

**本阶段不处理：** sync、run、Fargate/Service 的迁移，也不全局删除 sidecar 代码或配置。它们在过渡期间保留原路径，后续分别迁移。

## 2. 接管边界

- `NewInjector` 对非 Service 目标只返回 node-boundary 实现；Service 目标仍走 fargateInjector。
- `proxy.attachMode=sidecar` 对普通工作负载只产生 deprecated warning，不再回退为 sidecar；最终阶段删除该配置。
- manager 发布 `Virtual.agentListenerPort` 与 `NODE_ATTACHMENTS`，不为普通 proxy 创建或对齐 per-pod Job。
- 节点 agent 使用每节点单隧道；按 pod IP 做宿主机 DNAT；规则排除 `src=<agentPodIP>`，避免 agent origin-forwarding 被再次捕获。
- `hostNetwork=false`；hostPID/只读 `/proc` 与现有 node-agent 权限级别保持一致，PoC 已证明需要 `privileged: true`。
- OwnerID、header 路由、origin_cluster 和 leave 语义保持不变。
- 接管失败直接报错，不允许自动降级 sidecar。
- 已被 sync/run/Fargate sidecar 接管的 pod 不重复附加普通 node-agent。

## 3. 组件与配置

### manager

- 为每个 Virtual/workload 分配并持久化唯一 `agentListenerPort`（`20000-24999`）。
- admin 端口固定为 `agentListenerPort + 5000`。
- 根据 Virtual.Selector 和实际 pods 发布 NodeAttachment。
- 不创建 batch Job，不管理 agent pod 健康，不使用 `pods/exec`。

### 节点 agent

- 通过 Downward API 获取本节点 `nodeName` 与自身 `podIP`。
- watch 本节点 NodeAttachment，收敛宿主机规则。
- 每 workload 启动一个 envoy，xDS nodeID 继续使用现有 `GenEnvoyUID`。
- 每节点维持一条到 manager 的隧道，身份为 `OwnerID=nodeName, IsAgent=true`。
- 不进入业务 pod netns，不创建 per-pod 租约。

## 4. 生命周期与错误处理

- **pod 重建：** 只更新对应 NodeAttachment，不重启或替换业务 pod。
- **agent 重启：** 按 comment 清理并重建本机规则；验收要求 60 秒内恢复。
- **manager 重启：** ConfigMap 保留，agent 保留配置并重连隧道。
- **leave：** 删除附件与规则；没有 per-pod 隧道或租约需要清理。
- **配置失败/端口冲突/agent 未就绪：** 明确报错，不降级 sidecar。
- **旧 per-pod Job reconciler：** 在同一阶段切换时禁用，禁止双轨接管。

## 5. 测试与验收

### fake clientset 集成测试

- 非 Service 目标始终选择 node-boundary 实现；Service 目标仍选择 fargate。
- Virtual 端口分配唯一且持久，manager 重启不重新随机分配。
- NodeAttachment 随 pod 创建、重建、leave 正确发布与删除。
- sidecar-carrying pod 不发布附件。
- 不产生 per-pod Job，不要求 `pods/exec`。

### 真集群验收

- 普通 `TestFunctions` 与 `TestSSHFunctions` 的 proxy/mesh/central 用例在无 pin-sidecar 下全绿。
- full-proxy、mesh、portmap、SSH、central、leave→re-proxy 均覆盖。
- attach 前后 Deployment generation 与 pod UID 不变。
- 同节点多 workload、同 workload 多 pod、agent/manager/pod 重建无 churn 和端口冲突。
- Linux CI 超过 40 分钟按失败处理，输出 NodeAttachment、宿主机规则与 agent 日志。

## 6. 阶段完成定义与后续

本阶段只有在上述矩阵全绿后才算完成。随后分别为 sync、run、Fargate/Service 编写独立 spec 与实施计划。所有模式迁移完成后，最后统一删除 sidecar 代码、配置与兼容文档；本阶段不得提前做全局删除。
