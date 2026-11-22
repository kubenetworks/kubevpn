# Node-Agent 收敛式数据面 + 控制通道验证 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** 按 spec §4 落地收敛式数据面（A/B）与控制通道验证（C），删除整个 exec probe 机制。

**Architecture:** vpn 脚本收敛规则（清空重建）+ `kubevpn server` 长驻重申 + agent 身份经 GetTunIP 进入 lease store + reconciler 改用 lease 心跳校验。

**Spec:** docs/superpowers/specs/2026-11-22-nodeagent-dataplane-convergence-design.md §4

## Global Constraints

- 验证循环：`go build ./...` → `go vet ./pkg/...` → 相关包测试 → 最后 `make ut-compile`
- `cmd/` 冻结；非 integration 测试全部 fake；错误 `%w` 小写；`plog.G(ctx)`
- 不回退 Background propagation / findAgentPod 僵尸容忍（49401448b 的成果）

---

### Task 1: A — 收敛式 vpn 脚本

**Files:** Modify `pkg/inject/nodeagent_job.go`（vpnScript）；Test: 静态守卫测试加到现有 `pkg/inject/nodeagent_job_test.go`

- [ ] vpnScript 中两条 `-A` 规则前插入清空循环：

```sh
while iptables -t nat -D PREROUTING -p tcp ! -s 127.0.0.1 ! -d ${CIDR4} -j DNAT --to :15006 -m comment --comment "kubevpn-nodeagent" 2>/dev/null; do :; done
while ip6tables -t nat -D PREROUTING -p tcp ! -s ::1 ! -d ${CIDR6} -j DNAT --to :15006 -m comment --comment "kubevpn-nodeagent" 2>/dev/null; do :; done
```

（在既有 `-A` 行之前；`-A` 行保留——收敛语义 = 清空后恰好一条。）
- [ ] 新增守卫测试 `TestVPNScriptIsConvergent`：断言 vpnScript 同时包含清空循环与 `-A`，且清空循环在 `-A` 之前（strings.Index 比较）。
- [ ] `go build ./... && go vet ./pkg/inject/... && go test ./pkg/inject/ -run TestVPNScript -v` → 提交 `feat: convergent node-agent iptables rules (flush-by-comment then add)`

### Task 2: B — 长驻重申

**Files:** Modify `pkg/handler/connect_tun.go`（Run 内 env 门控新增重申 goroutine）；Modify `pkg/inject/nodeagent_job.go`（vpnScript export `KUBEVPN_AGENT_MAINTAIN_IPTABLES=1`）

- [ ] `Run` 中，当 `KUBEVPN_AGENT_MAINTAIN_IPTABLES == "1"` 时启动 ticker goroutine（10s 间隔，ctx 结束停止）：复用 cleanupIptables 的 exec 模式抽出的共享 helper `assertAgentIptables()`——按 `-S PREROUTING` 检查两个 kubevpn-nodeagent 规则是否齐全，缺失则用与 vpnScript 相同的规则 spec 逐条 `-A`（先跑 Task 1 的清空循环再补两条，保证幂等）。
- [ ] helper 拆分：`flushAgentIptables()`（清空）/`applyAgentIptables()`（加两条）/`assertAgentIptables()`（flush+apply）；cleanupIptables 改为调用 flushAgentIptables。
- [ ] 测试：`TestAssertAgentIptablesArgs` 断言 applyAgentIptables 生成的 exec 参数与 vpnScript 中的规则 spec 逐 token 一致（把规则 spec 提为包级常量 `agentDNATRuleSpec4/6`，vpnScript 与 Go helper 共用同一常量拼装，杜绝漂移）。
- [ ] vpnScript 增加 `export KUBEVPN_AGENT_MAINTAIN_IPTABLES=1`（与 CLEANUP 并列）。
- [ ] `go build ./... && go vet ./pkg/handler/... ./pkg/inject/... && go test ./pkg/handler/ -run TestAssertAgent -v && go test ./pkg/inject/ -run TestVPNScript -v` → 提交 `feat: agent re-asserts DNAT rules periodically while serving`

### Task 3: C1 — agent 身份进入 lease store

**Files:** 追踪后定（预计 `pkg/inject/nodeagent_job.go` URL、`pkg/config`/`pkg/endpoint` 的 -l URL 解析、agent 侧 GetTunIP 调用点、`pkg/xds/tun_config.go:82`）

- [ ] 追踪：agent vpn 容器 `kubevpn server -l tun:/tcp://...:10801?route=${CIDR4}` 发起的租约请求（CI 日志 "owner unknown isAgent=false"）在哪一侧构造 TunIPRequest。从 CI 日志反查：manager 侧 dhcp.go:304/`leases/ipalloc.go:152` 的调用链。
- [ ] 实现：vpnScript 的 URL 追加 `&agent=${POD_UID}`；`kubevpn server` 解析该 query 参数，在它的 GetTunIP 调用中设 `OwnerID=<podUID>`、`IsAgent=true`；lease store 照常记录（字段已有）。
- [ ] 约束：非 agent 连接（普通用户 kubevpn connect 的数据面）不受影响——仅当 URL 带 agent 参数时才改写身份。
- [ ] 测试：URL 解析单测（agent 参数 → 身份字段）；如租约路径有 fake 可测的接缝则补一条端到端 fake 测试。
- [ ] `go build ./... && go vet ./pkg/... && 相关包测试` → 提交 `feat: agent tunnel identifies itself via lease (pod UID + IsAgent)`

### Task 4: C2 — reconciler 改用 lease 心跳校验，删除 exec 机制

**Files:** Modify `pkg/xds/agent_verify.go`（重写为 lease 查询）、`pkg/xds/agent_reconciler.go`、`pkg/xds/tun_config.go`（接线）、`pkg/cluster/manager/resources.go` genRole、`charts/kubevpn/templates/role.yaml`（回退 pods/exec）、删除相关旧测试

- [ ] `TunConfigServer` 向 `AgentReconciler` 注入 `agentSessionAlive func(podUID string) bool`（in-process 读 lease store：该 owner 的分配 IsAgent=true 且 LastRenew 距今 <30s；agent 心跳 5s）。
- [ ] `verifyDataPlane` 重写：`return r.agentSessionAlive == nil || r.agentSessionAlive(podUID)`；删除 probeDNAT/dnatProbeCommand/parseDNATCount/isNumeric/lastVerify exec 节流与 `probe` 注入字段。
- [ ] 删除 vpnScript 的 `echo $$ > /tmp/kubevpn-agent-target-pid`（Task 1/2 已改的脚本随 T3 URL 一起调整）。
- [ ] genRole 与 chart role.yaml 回退 `pods/exec` 规则（RBAC 断言测试同步回 4 条规则——注意与 T3 无关的另一条 pods list 规则保留）。
- [ ] 测试：`TestReconcileOnce_KeepsJobWhileAgentLeaseAlive`（stub 返回 true → 不删）、`TestReconcileOnce_RecreatesJobWhenLeaseStale`（stub 返回 false → 删+建，复用 delete-assertion reactor 模式）；删除 TestParseDNATCount/TestDNATProbeCommandAlwaysExitsZero/TestReconcileOnce_RecreatesWhenDNATProbeSaysMissing/KeepsJobWhenDNATProbeSaysPresent。
- [ ] `go build ./... && go vet ./pkg/... && go test ./pkg/xds/ ./pkg/cluster/manager/ && make ut-compile` → 提交 `refactor: verify agent dataplane via lease heartbeat, drop exec probe`

### Task 5: docs + 全量验证 + 推送

**Files:** `docs/55-node-hosted-agent.md` §19、spec §4 状态

- [ ] docs/55 §19 补记第二轮架构升级（收敛规则/长驻重申/lease 验证/exec 机制删除/Background propagation 根因）。
- [ ] spec §4 标注 implemented。
- [ ] `go build ./... && go vet ./pkg/... && go test ./pkg/... && make ut-compile` 全绿 → 提交 docs → push。
