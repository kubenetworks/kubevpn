# Node-Agent 数据面声明式收敛 — 设计文档

- **日期:** 2026-11-22
- **状态:** implemented
- **证据基线:** PR #822 CI run [44899084723](https://github.com/kubenetworks/kubevpn/actions/runs/44899084723)（commit 292ae1822）日志取证
- **关联文档:** docs/55-node-hosted-agent.md（§12 假设验证表、§16 修复记录将在实施时同步更新）

## 1. 背景与诊断结论

PR #822 的 TestSSHFunctions 07-12（central：manager 部署在 kubevpn ns）持续失败，
此前被误诊为"node-agent + SSH 的 mesh 数据面缺口"。本轮日志取证推翻了该结论：

### 1.1 node-agent + SSH 拦截链路本身是通的（证据）

11:43:30 的 agent envoy access log：

```
[envoy-access] "GET /health" code=200 route=- cluster=198.18.0.1_9080 upstream=198.18.0.1:9080 env=test
```

请求经 目标 pod netns PREROUTING DNAT(:15006) → agent envoy（与 vpn 容器同经
`kubevpn agent` setns 进入目标 pod netns）→ 匹配 env=test 路由 → 198.18.0.1:9080
（用户 TUN IP）→ agent vpn 容器 `kubevpn server` TUN 隧道 → manager → SSH 中继 →
用户本地 server，返回 200。**机制完整工作**，"SSH 数据面缺口"不成立。

### 1.2 D1 — connect 全量重建 manager，覆盖 attach mode（已证实，产品 bug）

时间线（07-central-connect）：

| 时刻 | 事件 |
|---|---|
| 11:42:52 | install 创建 manager（env=node-agent 默认） |
| 11:42:59 | 测试将 `KUBEVPN_PROXY_ATTACH_MODE=sidecar` pin 到 deploy（0.01s） |
| 11:43:00 | connect：install 的 pod 尚未 Running → `CleanupTrafficManagerResources` 全删重建 |
| 11:43:01 | 新 manager pod env=`node-agent`（pod describe 直接可见） |

覆盖点：`pkg/cluster/manager/manager.go:150` — `createOutboundPod` 重建时硬编码
`config.AttachModeNodeAgent` 传给 `genDeploySpec`。01-06 通过仅因它们的 pin 在
connect **之后**才打。真实用户场景：helm 设 `proxy.attachMode=sidecar` 后跑
`kubevpn connect`，值被静默覆盖。

### 1.3 D2 — leave→re-proxy 循环使拦截静默失效（09 失败根因）

09-central-proxy-mesh 时间线：

| 时刻 | 事件 |
|---|---|
| 11:43:25 | 08 full-proxy inject → agent job（z5pww，pod UID 30ab58c6）→ 11:43:26 DNAT 生效 |
| 11:43:30 | 拦截链路验证 200（见 §1.1） |
| 11:44:08-25 | 09 leave：`removeEnvoyConfig` + `deleteAgentJobs`；job 删除后等 10s 超时（"timeout waiting for agent pods to terminate, returning anyway"），但 agent pod 与 reviews pod 此后持续 Running、restartCount=0 |
| 11:44:27 | 09 mesh inject：16ms 完成，xDS 规则正确（env=test），**但 agent job/pod 未按预期重建**（11:50 dump 中无新建 pod） |
| 11:45:23+ | mesh curl → 响应=remote（bypass envoy）；envoy access log 至 dump（11:50）止于 11:43:30 |
| 11:50 | dump：reviews pod bts7p 1/1 Running 12m；agent pod z5pww 2/2 Running 7m13s；仅此一个 agent job |

**结构性问题：不变量无仲裁者。** "凡 isAgent Virtual 选中的存活 pod，其 netns 必有
生效的 DNAT+envoy"这一不变量，inject/leave 各自 best-effort，reconciler 只对齐
job 对象存在性（`pkg/xds/agent_reconciler.go` reconcileOnce：`Selector==""` 静默
跳过、单项错误中止整轮、30s tick、不校验 agent pod 状态与数据面实际生效）。
leave 与 re-inject 之间的任何时序抖动都会造成"现场全绿但拦截已死"。

### 1.4 次生失败同源

- 10（kubevpnLeave 19s：LeaveInject 拨 manager:9002 超时重试）、11（sync 后同样
  leave/re-inject 循环）、12：同族级联。
- 07/08 `centerCheck*Status` 0.06s 失败：测试断言过期（期望 Namespace=default，
  实际连接在 kubevpn ns）。

## 2. 设计

### §1 核心不变量与 reconciler 重构

**不变量**：存在 `isAgent=true` 且 `Selector≠""` 的 Virtual ⇒ 其选中的每个 Running
pod 满足：① 有 agent Job；② agent pod Running/Ready；③ 目标 pod netns 存在
`kubevpn-nodeagent` 标记的 DNAT 规则。

`AgentReconciler`（pkg/xds/agent_reconciler.go）改造：

- tick 30s → 5s；reconcileOnce 逐条目隔离错误（单项失败 log 后继续，不再中止整轮）
- 校验升级为三级：Job 存在 → agent pod Running/Ready → DNAT 实测存在
- DNAT 实测原语：`exec` 进 agent pod 执行 `iptables -t nat -S` —— agent 容器主进程
  已 setns 进目标 pod netns，exec 会话落在目标 netns，可直接验证规则。节流：仅当
  本轮 reconcile 有变更，或距上次实测 ≥60s 时执行
- ①②③ 任一失败 → 删 Job → 重建（复用 `GenAgentJobSpec` / `AgentJobInput`）
- RBAC：manager 的 `genRole` 补 `pods/exec`

### §2 inject 同步就绪 + leave 收尾

- `EnsureAgentJob`（pkg/inject/nodeagent.go）：Create/AlreadyExists 处理后轮询等待
  agent pod Running/Ready（上限 30s、500ms 间隔），超时返回 error——ProxyInject 将
  失败带给客户端。消灭"inject 返回但数据面不存在"
- `deleteAgentJobs`（pkg/inject/mesh.go）：保留 best-effort 等待不变；§2 的同步等待
  使 leave→re-inject 循环天然安全

### §3 D1：attach mode 继承

- `createOutboundPod`：在 `CleanupTrafficManagerResources` **之前** GET 现有 deploy，
  读出 `KUBEVPN_PROXY_ATTACH_MODE` 传入 `genDeploySpec`（签名已支持，manager.go:150
  硬编码点移除）
- 对"pod 存在但未 Running"增加 ≤10s 等待再决定重建，缩小竞态触发面
- Upgrade 路径 `existingAttachMode` 保留逻辑不动（单测已验证）

### §4 测试

- D3 修正：`centerCheck*Status` 断言 Namespace=default → kubevpn
- 新增集成测试（fake.NewSimpleClientset，多阶段故事）：
  1. `TestAgentReconciler_Converges`：pod 重建 → 两个 tick 内 job 删+建；校验失败 → 重建
  2. `TestEnsureAgentJob_WaitsForReady`：pod 未 Ready → 注入阻塞至 Ready；terminating
     job → 等待后重建
  3. `TestCreateOutboundPod_PreservesAttachMode`：existing deploy env=sidecar +
     pod 未 Running → 重建后 env 仍 sidecar
  4. 09 回归故事：inject → leave → re-inject → 断言 job/pod 状态链完整
- e2e：TestSSHFunctions 07-12 修复后应自然转绿（不改动测试结构，仅 §4 断言修正）

## 3. 决策记录

- **方案选择**：A（声明式收敛）> B（最小时序修补，静默失效可能复发）> C（SSH+mesh
  强制降级 sidecar，回退已验证可用的 node-agent 路径，无必要）。
- **误诊修正**：撤销"node-agent + SSH mesh 数据面缺口"的 Known limitation 定性；
  docs/55 §16 相应条目在本实施中改写为"生命周期竞态，已由声明式收敛修复"。
- **遗留不确定点**：D2 的微观竞态（leave 的 Job.Delete 与 inject 的 EnsureAgentJob
  之间哪一步吞掉了重建）不再单独排查——§2 同步等待 + §1 reconciler 校验将其结构性
  消灭；e2e 转绿即为验证。
