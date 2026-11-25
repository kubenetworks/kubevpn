# Sidecar Removal Phase 1 — 普通 Proxy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让普通非 Service 工作负载的 proxy/connect 完全使用 node-boundary DaemonSet，彻底移除该路径对 sidecar 和 per-pod Job 的依赖。

**Architecture:** manager 分配并持久化 Virtual.agentListenerPort，发布 NODE_ATTACHMENTS；每节点一个无 hostNetwork 的 agent，按目标 pod IP 在宿主机 netns 收敛 DNAT 到 agent Pod IP，使用每节点单隧道和每 workload 独立 Envoy 进程。旧 per-pod Job reconciler 在同一阶段禁用。

**Tech Stack:** Go 1.26, client-go informers/fake clientset, sigs.k8s.io/yaml, iptables/nsenter, Envoy, Cobra。

**Specs:**
- `docs/superpowers/specs/2026-11-25-sidecar-removal-phase1-design.md`
- `docs/superpowers/specs/2026-11-23-node-boundary-agent-redesign.md`
- `docs/superpowers/poc/2026-11-23-node-boundary-result.md`

## Global Constraints

- 非 Service 工作负载不得使用 sidecar，也不得在失败时自动降级 sidecar。
- 不修改业务 workload spec，不滚动重启业务 pod。
- `hostNetwork=false`；每节点一个 DaemonSet agent，每节点一条隧道；不创建 per-pod Job。
- 宿主机 DNAT 精确匹配目标 pod IP，并排除 `src=<agentPodIP>`。
- sync/run/Fargate/Service 的现有路径本阶段不动；sidecar 只做过渡保留，最后才全局删除。
- `cmd/` 现有命令冻结；新增隐藏子命令允许。不得修改现有命令参数/行为/输出。
- 非 integration 测试不得访问真实集群；fake clientset 用于控制面集成测试。
- 每个任务运行 `go build ./...`、相关 `go vet` 与测试；最后运行 `make ut-compile` 和真集群矩阵。
- 日志使用 `plog.G(ctx)`，错误小写、无尾标点、`%w` 包装。
- Linux CI 超过 40 分钟按失败处理，立即输出附件、宿主机规则与 agent 日志。

## File Structure

| File | Responsibility |
|---|---|
| `pkg/controlplane/virtual.go` | Virtual 增加 AgentListenerPort |
| `pkg/controlplane/node_attachment.go` | NodeAttachment 类型与 agent 端口分配 |
| `pkg/config/config.go` | `KeyNodeAttachments` 配置 key |
| `pkg/xds/attachment_publisher.go` | manager 期望状态发布，替代 Job reconciler |
| `pkg/xds/tun_config.go`, `pkg/xds/xds.go` | 发布器接线 |
| `pkg/nodeagent/iptables.go` | 宿主机 DNAT 收敛 |
| `pkg/nodeagent/runtime.go`, `supervisor.go` | 节点 agent 主循环与 Envoy 监督 |
| `pkg/cluster/manager/nodeagent_daemonset.go` | DaemonSet spec/ensure |
| `pkg/inject/envoy.go` | 可配置 admin 端口的 bootstrap |
| `pkg/inject/nodeagent.go`, `injector.go` | 普通目标切换，不再创建 Job |
| `pkg/xds/cache.go` | agent listener/UDP 端口接入 |
| `cmd/kubevpn/cmds/nodeagent.go` | 新隐藏 `node-agent` 子命令 |
| `cmd/kubevpn/cmds/root.go` | 仅注册新子命令 |

---

### Task 1: Virtual/NodeAttachment 模型与端口分配

**Files:**
- Modify: `pkg/controlplane/virtual.go`
- Modify: `pkg/config/config.go`
- Create: `pkg/controlplane/node_attachment.go`
- Test: `pkg/controlplane/node_attachment_test.go`

**Interfaces:**
- Produces: `NodeAttachment` struct；`EnsureAgentListenerPorts(virtuals []*Virtual) (bool, error)`；`config.KeyNodeAttachments = "NODE_ATTACHMENTS"`；端口范围 `20000-24999`。

- [ ] **Step 1: 写失败测试**

```go
func TestEnsureAgentListenerPorts_UniqueAndStable(t *testing.T) {
	virtuals := []*Virtual{
		{UID: "deployments.apps.reviews", Selector: "app=reviews", Rules: []*Rule{{IsAgent: true}}},
		{UID: "deployments.apps.details", Selector: "app=details", Rules: []*Rule{{IsAgent: true}}},
		{UID: "deployments.apps.old", Selector: "app=old", Rules: []*Rule{{IsAgent: true}}, AgentListenerPort: 20100},
	}
	changed, err := EnsureAgentListenerPorts(virtuals)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected new ports to be assigned")
	}
	seen := map[int32]bool{}
	for _, v := range virtuals {
		if v.AgentListenerPort < 20000 || v.AgentListenerPort > 24999 {
			t.Fatalf("port out of range: %d", v.AgentListenerPort)
		}
		if seen[v.AgentListenerPort] {
			t.Fatalf("duplicate port: %d", v.AgentListenerPort)
		}
		seen[v.AgentListenerPort] = true
	}
	if virtuals[2].AgentListenerPort != 20100 {
		t.Fatal("existing port changed")
	}
	changed, err = EnsureAgentListenerPorts(virtuals)
	if err != nil || changed {
		t.Fatalf("second allocation must be stable: changed=%v err=%v", changed, err)
	}
}

func TestEnsureAgentListenerPorts_SkipsSidecar(t *testing.T) {
	v := &Virtual{UID: "deployments.apps.reviews", Rules: []*Rule{{IsAgent: false}}}
	changed, err := EnsureAgentListenerPorts([]*Virtual{v})
	if err != nil || changed || v.AgentListenerPort != 0 {
		t.Fatalf("sidecar virtual changed: changed=%v err=%v port=%d", changed, err, v.AgentListenerPort)
	}
}
```

- [ ] **Step 2: 确认失败**

Run: `go test ./pkg/controlplane/ -run TestEnsureAgentListenerPorts -v`
Expected: FAIL — `EnsureAgentListenerPorts` undefined。

- [ ] **Step 3: 实现模型与分配器**

`pkg/controlplane/virtual.go` 在 Virtual 中增加：

```go
	// AgentListenerPort is the unique per-workload listener used by the
	// node-boundary agent. Zero means sidecar/legacy mode.
	AgentListenerPort int32 `yaml:"agentListenerPort,omitempty" json:"agentListenerPort,omitempty"`
```

`pkg/config/config.go` 增加：

```go
	// KeyNodeAttachments stores desired per-pod node-boundary attachments.
	KeyNodeAttachments = "NODE_ATTACHMENTS"
```

`pkg/controlplane/node_attachment.go`：

```go
package controlplane

import "fmt"

const (
	AgentListenerPortMin int32 = 20000
	AgentListenerPortMax int32 = 24999
)

// NodeAttachment is the desired attachment of one target pod to the agent on
// its node. It is declarative desired state, not a per-pod process/lease record.
type NodeAttachment struct {
	PodUID           string `json:"podUID" yaml:"podUID"`
	Namespace        string `json:"namespace" yaml:"namespace"`
	NodeName         string `json:"nodeName" yaml:"nodeName"`
	PodIP            string `json:"podIP" yaml:"podIP"`
	WorkloadUID      string `json:"workloadUID" yaml:"workloadUID"`
	AgentListenerPort int32 `json:"agentListenerPort" yaml:"agentListenerPort"`
}

// EnsureAgentListenerPorts assigns stable, unique listener ports to agent
// Virtuals. It never changes a valid existing assignment.
func EnsureAgentListenerPorts(virtuals []*Virtual) (bool, error) {
	used := make(map[int32]bool)
	for _, v := range virtuals {
		if v.AgentListenerPort != 0 {
			used[v.AgentListenerPort] = true
		}
	}
	changed := false
	for _, v := range virtuals {
		agent := false
		for _, rule := range v.Rules {
			if rule != nil && rule.IsAgent {
				agent = true
				break
			}
		}
		if v.Selector == "" || !agent {
			continue
		}
		if v.AgentListenerPort != 0 {
			continue
		}
		var port int32
		for port = AgentListenerPortMin; port <= AgentListenerPortMax; port++ {
			if !used[port] {
				break
			}
		}
		if port > AgentListenerPortMax {
			return changed, fmt.Errorf("no available agent listener ports")
		}
		v.AgentListenerPort = port
		used[port] = true
		changed = true
	}
	return changed, nil
}
```

- [ ] **Step 4: 验证并提交**

```bash
go test ./pkg/controlplane/ -run TestEnsureAgentListenerPorts -v
go build ./...
go vet ./pkg/controlplane/...
git add pkg/controlplane/virtual.go pkg/controlplane/node_attachment.go pkg/controlplane/node_attachment_test.go pkg/config/config.go
git commit -m "feat: add node-boundary attachment model and listener port allocation

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

### Task 2: manager 用 AttachmentPublisher 替代 Job reconciler

**Files:**
- Rename: `pkg/xds/agent_reconciler.go` → `pkg/xds/attachment_publisher.go`（使用 `git mv`）
- Create: `pkg/xds/attachment_publisher_test.go`
- Modify: `pkg/xds/tun_config.go`, `pkg/xds/xds.go`
- Delete: `pkg/xds/agent_reconciler_test.go`（旧 Job 行为测试，由新测试替代）

**Interfaces:**
- Consumes: `EnsureAgentListenerPorts`、`NodeAttachment`、`config.KeyNodeAttachments`。
- Produces: `NewAttachmentPublisher(clientset kubernetes.Interface, managerNamespace string) *AttachmentPublisher`；`Start(ctx) error`；`reconcileOnce(ctx) error`；5s reconcile tick。

- [ ] **Step 1: 写失败集成测试**

测试环境用 fake clientset + manager ConfigMap，Virtual 包含 `Selector: app=reviews` 和 `IsAgent:true`；创建 running pod（`UID=uid-1`、`NodeName=node-a`、`PodIP=10.244.0.5`、`Namespace=default`）。

```go
func TestAttachmentPublisher_PublishesAndRemoves(t *testing.T) {
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "reviews-x", Namespace: "default", UID: "uid-1", Labels: map[string]string{"app": "reviews"}},
		Spec:       corev1.PodSpec{NodeName: "node-a"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.0.5"},
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
		Data: map[string]string{config.KeyEnvoy: `- schemaVersion: 2
  Namespace: default
  Uid: deployments.apps.reviews
  selector: app=reviews
  Rules:
  - ownerID: owner-1
    LocalTunIPv4: 198.18.0.1
    isAgent: true
`},
	}
	clientset := fake.NewSimpleClientset(cm, pod)
	p := NewAttachmentPublisher(clientset, "kubevpn")
	if err := p.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := clientset.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var attachments []controlplane.NodeAttachment
	if err := yaml.Unmarshal([]byte(got.Data[config.KeyNodeAttachments]), &attachments); err != nil {
		t.Fatal(err)
	}
	if len(attachments) != 1 || attachments[0].PodUID != "uid-1" || attachments[0].NodeName != "node-a" || attachments[0].PodIP != "10.244.0.5" {
		t.Fatalf("unexpected attachments: %+v", attachments)
	}
	if attachments[0].AgentListenerPort == 0 {
		t.Fatal("listener port not allocated")
	}

	// Target pod deletion removes its attachment on the next pass.
	_ = clientset.CoreV1().Pods("default").Delete(ctx, pod.Name, metav1.DeleteOptions{})
	if err := p.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = clientset.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	attachments = nil
	_ = yaml.Unmarshal([]byte(got.Data[config.KeyNodeAttachments]), &attachments)
	if len(attachments) != 0 {
		t.Fatalf("stale attachments: %+v", attachments)
	}
}
```

- [ ] **Step 2: 确认失败**

Run: `go test ./pkg/xds/ -run TestAttachmentPublisher -v`
Expected: FAIL — `NewAttachmentPublisher` undefined。

- [ ] **Step 3: 实现发布器**

执行 `git mv pkg/xds/agent_reconciler.go pkg/xds/attachment_publisher.go`，用以下结构替换 Job 逻辑（文件内保留必要 imports 与现有 ConfigMap 读取逻辑）：

```go
// AttachmentPublisher publishes desired NodeAttachments. It does not create
// Jobs, inspect agent pods, or own any per-pod data-plane lifecycle.
type AttachmentPublisher struct {
	clientset        kubernetes.Interface
	managerNamespace string
	interval         time.Duration
}

func NewAttachmentPublisher(clientset kubernetes.Interface, managerNamespace string) *AttachmentPublisher {
	return &AttachmentPublisher{clientset: clientset, managerNamespace: managerNamespace, interval: 5 * time.Second}
}

func (p *AttachmentPublisher) Start(ctx context.Context) error {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		if err := p.reconcileOnce(ctx); err != nil {
			plog.G(ctx).Warnf("[AttachmentPublisher] reconcile failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
```

`reconcileOnce` 必须按以下顺序实现：
1. 读取并解析 `KeyEnvoy` 为 `[]*controlplane.Virtual`。
2. 调用 `EnsureAgentListenerPorts`；若返回 changed，将 Virtual YAML 写回 `KeyEnvoy`。
3. 对每个 `Selector != ""` 且含 IsAgent rule 的 Virtual，按 namespace/selector list pods。
4. 跳过非 Running、Deleting、无 PodIP、无 NodeName，以及带 `config.ContainerSidecarVPN`/`config.ContainerSidecarEnvoy` 的 pod。
5. 生成 `NodeAttachment{PodUID:string(pod.UID), Namespace:pod.Namespace, NodeName:pod.Spec.NodeName, PodIP:pod.Status.PodIP, WorkloadUID:v.UID, AgentListenerPort:v.AgentListenerPort}`。
6. 用 `yaml.Marshal` 写回 `KeyNodeAttachments`；内容相同则不 Update。
7. 单个 workload list 失败只 warn 并继续，ConfigMap 读取/写入失败返回 error。
8. 全部完成后返回 nil；不得调用任何 BatchV1().Jobs API。

`tun_config.go` 将 `agentReconciler` 字段改为 `attachmentPublisher *AttachmentPublisher`，构造 `NewAttachmentPublisher(clientset, namespace)`；`xds.go` 改为启动 `tunConfig.attachmentPublisher`，保留 `nil` 时 disabled 分支。

- [ ] **Step 4: 验证并提交**

```bash
go test ./pkg/xds/ -run TestAttachmentPublisher -v
go build ./...
go vet ./pkg/xds/...
git add pkg/xds/attachment_publisher.go pkg/xds/attachment_publisher_test.go pkg/xds/tun_config.go pkg/xds/xds.go pkg/xds/agent_reconciler_test.go
git commit -m "refactor: publish node attachments instead of managing per-pod jobs

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

### Task 3: 宿主机 DNAT 收敛器

**Files:**
- Create: `pkg/nodeagent/iptables.go`
- Create: `pkg/nodeagent/iptables_test.go`

**Interfaces:**
- Consumes: `controlplane.NodeAttachment`。
- Produces: `CommandRunner`、`ConvergeHostDNAT(ctx context.Context, runner CommandRunner, agentIP string, attachments []controlplane.NodeAttachment) error`。

- [ ] **Step 1: 写失败测试**

使用 fake runner 记录命令，并让 `iptables -t nat -S PREROUTING` 返回已有同 comment 规则，验证旧规则先被删除，再添加新规则；第二次收敛不得产生重复。

```go
func TestConvergeHostDNAT_ConvergesAndExcludesAgent(t *testing.T) {
	runner := &fakeRunner{listOutput: "-A PREROUTING -p tcp ! -s 10.244.0.8 -d 10.244.0.5 -j DNAT --to 10.244.0.8:20001 -m comment --comment \"kubevpn-nodeagent\"\n"}
	attachments := []controlplane.NodeAttachment{{
		PodIP: "10.244.0.5", AgentListenerPort: 20001,
	}}
	if err := ConvergeHostDNAT(context.Background(), runner, "10.244.0.8", attachments); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(runner.commands(), "\n")
	if !strings.Contains(joined, "-D PREROUTING") {
		t.Fatal("old marked rule was not flushed")
	}
	if !strings.Contains(joined, "! -s 10.244.0.8 -d 10.244.0.5") {
		t.Fatal("agent source exclusion or exact podIP match missing")
	}
	if !strings.Contains(joined, "-p udp") {
		t.Fatal("UDP DNAT rule missing")
	}
	if !strings.HasPrefix(joined, "nsenter --net=/host/proc/1/ns/net") {
		t.Fatal("rule was not applied in host netns")
	}
}
```

- [ ] **Step 2: 确认失败**

Run: `go test ./pkg/nodeagent/ -run TestConvergeHostDNAT -v`
Expected: FAIL — 类型/函数不存在。

- [ ] **Step 3: 实现收敛器**

```go
package nodeagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
)

// CommandRunner makes host-netns command execution testable without a cluster.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ConvergeHostDNAT flushes only kubevpn-marked rules, then installs exact
// podIP → agentIP:port rules for TCP and UDP. The agent-source exclusion is
// mandatory: otherwise the agent's origin-forwarding is captured again.
func ConvergeHostDNAT(ctx context.Context, runner CommandRunner, agentIP string, attachments []controlplane.NodeAttachment) error {
	if runner == nil || agentIP == "" {
		return fmt.Errorf("runner and agent IP are required")
	}
	const prefix = "nsenter"
	const hostNet = "--net=/host/proc/1/ns/net"
	list, err := runner.Run(ctx, prefix, hostNet, "iptables", "-t", "nat", "-S", "PREROUTING")
	if err != nil {
		return fmt.Errorf("list host PREROUTING rules: %w", err)
	}
	for _, line := range strings.Split(list, "\n") {
		if !strings.Contains(line, config.AgentIptablesComment) {
			continue
		}
		spec := strings.TrimSpace(strings.TrimPrefix(line, "-A "))
		if spec == "" {
			continue
		}
		args := []string{hostNet, "iptables", "-t", "nat", "-D"}
		args = append(args, strings.Fields(spec)...)
		if _, err := runner.Run(ctx, prefix, args...); err != nil {
			return fmt.Errorf("delete marked host rule: %w", err)
		}
	}
	for _, attachment := range attachments {
		for _, protocol := range []string{"tcp", "udp"} {
			args := []string{hostNet, "iptables", "-t", "nat", "-A", "PREROUTING",
				"-p", protocol, "!", "-s", agentIP, "-d", attachment.PodIP,
				"-j", "DNAT", "--to", fmt.Sprintf("%s:%d", agentIP, attachment.AgentListenerPort),
				"-m", "comment", "--comment", config.AgentIptablesComment,
			}
			if _, err := runner.Run(ctx, prefix, args...); err != nil {
				return fmt.Errorf("install host DNAT for pod %s: %w", attachment.PodIP, err)
			}
		}
	}
	return nil
}
```

生产 runner 用 `exec.CommandContext(...).CombinedOutput()`；测试 fake 只记录参数，不执行 iptables。

- [ ] **Step 4: 验证并提交**

```bash
go test ./pkg/nodeagent/ -run TestConvergeHostDNAT -v
go build ./...
go vet ./pkg/nodeagent/...
git add pkg/nodeagent/iptables.go pkg/nodeagent/iptables_test.go
git commit -m "feat: converge host-side pod DNAT rules for node-boundary agent

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

### Task 4: 节点 agent 运行时与 Envoy 进程监督

**Files:**
- Create: `pkg/nodeagent/runtime.go`, `pkg/nodeagent/supervisor.go`
- Create: `pkg/nodeagent/supervisor_test.go`
- Modify: `pkg/inject/envoy.go`
- Create: `cmd/kubevpn/cmds/nodeagent.go`
- Modify: `cmd/kubevpn/cmds/root.go`（仅注册新命令）

**Interfaces:**
- Consumes: `ConvergeHostDNAT`、`NodeAttachment`、`inject.RenderEnvoyBootstrap`。
- Produces: `nodeagent.Run(ctx, Options) error`；`Options{Clientset, ManagerNamespace, NodeName, PodIP, Image string}`；`inject.RenderEnvoyBootstrapWithAdmin(ctx, managerAddr string, adminPort int32)`；新隐藏 `node-agent` 子命令。

- [ ] **Step 1: 写失败测试**

纯函数测试不启动 Envoy：

```go
func TestEnvoyLaunchSpec_UniquePortsAndBaseID(t *testing.T) {
	a := envoyLaunchSpec("deployments.apps.reviews", 20001)
	b := envoyLaunchSpec("deployments.apps.details", 20002)
	if a.adminPort != 25001 || b.adminPort != 25002 {
		t.Fatalf("unexpected admin ports: %d %d", a.adminPort, b.adminPort)
	}
	if a.baseID == b.baseID {
		t.Fatal("multiple Envoys must not share base-id")
	}
	if a.listenerPort != 20001 || b.listenerPort != 20002 {
		t.Fatal("listener port not preserved")
	}
}
```

- [ ] **Step 2: 确认失败**

Run: `go test ./pkg/nodeagent/ -run TestEnvoyLaunchSpec -v`
Expected: FAIL — `envoyLaunchSpec` undefined。

- [ ] **Step 3: 实现运行时**

`pkg/nodeagent/supervisor.go` 定义：

```go
type launchSpec struct {
	workloadUID  string
	listenerPort int32
	adminPort    int32
	baseID       int32
}

func envoyLaunchSpec(workloadUID string, listenerPort int32) launchSpec {
	return launchSpec{
		workloadUID:  workloadUID,
		listenerPort: listenerPort,
		adminPort:    listenerPort + 5000,
		baseID:       listenerPort - controlplane.AgentListenerPortMin + 1,
	}
}
```

`Supervisor.Sync` 按 WorkloadUID 分组：新增 workload 启动 Envoy，消失 workload 停止 Envoy，已有配置不变则不重启。启动参数必须包含 `--service-node <GenEnvoyUID>`、`--service-cluster <同值>`、`--base-id <唯一值>`；bootstrap admin 端口使用 `listenerPort+5000`。进程意外退出时以有界 backoff 重启；context 结束时停止全部进程。日志写 `/tmp/kubevpn-nodeagent-<workloadUID>.log`，不得静默吞掉启动错误。

`pkg/inject/envoy.go` 增加：

```go
// RenderEnvoyBootstrapWithAdmin is the node-boundary variant with an isolated
// admin port; the legacy RenderEnvoyBootstrap keeps the existing 9003 behavior.
func RenderEnvoyBootstrapWithAdmin(ctx context.Context, managerAddr string, adminPort int32) ([]byte, error) {
	b, err := RenderEnvoyBootstrap(ctx, managerAddr)
	if err != nil {
		return nil, err
	}
	if adminPort == 9003 {
		return b, nil
	}
	return []byte(strings.Replace(string(b), "port_value: 9003", fmt.Sprintf("port_value: %d", adminPort), 1)), nil
}
```

`pkg/nodeagent/runtime.go` 的 `Run`：
1. 使用 informer watch manager ConfigMap。
2. 每次变更读取 `KeyNodeAttachments`，筛选 `NodeName == Options.NodeName` 且 PodIP 非空。
3. 调用 `ConvergeHostDNAT` 和 `Supervisor.Sync`。
4. 启动并维持一个子进程：

```text
kubevpn server --debug -l tun:/tcp://<manager>:10801?route=198.18.0.0/16&agent=<NodeName>
```

复用现有 `?agent=` 身份和心跳。子进程退出时以退避重连；context 结束时关闭隧道与所有 Envoy。

`cmd/kubevpn/cmds/nodeagent.go` 创建隐藏命令 `node-agent`，从 `NODE_NAME`、`POD_IP`、`MANAGER_NAMESPACE`、`IMAGE` 读取配置，使用 in-cluster config 构造 clientset，然后调用 `nodeagent.Run`。在 root.go 仅增加一行注册。

- [ ] **Step 4: 验证并提交**

```bash
go test ./pkg/nodeagent/ -run 'TestEnvoyLaunchSpec|TestConvergeHostDNAT' -v
go test ./pkg/inject/ -run TestRenderEnvoyBootstrap -v
go build ./...
go vet ./pkg/nodeagent/... ./pkg/inject/...
git add pkg/nodeagent/runtime.go pkg/nodeagent/supervisor.go pkg/nodeagent/supervisor_test.go pkg/inject/envoy.go cmd/kubevpn/cmds/nodeagent.go cmd/kubevpn/cmds/root.go
git commit -m "feat: add node-boundary agent runtime and envoy supervisor

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

### Task 5: DaemonSet spec 与 manager 部署接线

**Files:**
- Create: `pkg/cluster/manager/nodeagent_daemonset.go`
- Create: `pkg/cluster/manager/nodeagent_daemonset_test.go`
- Modify: `pkg/cluster/manager/manager.go`

**Interfaces:**
- Produces: `genNodeAgentDaemonSet(namespace, image string) *appsv1.DaemonSet`；`ensureNodeAgentDaemonSet(ctx, clientset, namespace, image) error`。

- [ ] **Step 1: 写失败测试**

```go
func TestGenNodeAgentDaemonSet_SecurityContract(t *testing.T) {
	ds := genNodeAgentDaemonSet("kubevpn", "test-image")
	podSpec := ds.Spec.Template.Spec
	if podSpec.HostNetwork {
		t.Fatal("hostNetwork must be disabled")
	}
	if !podSpec.HostPID {
		t.Fatal("hostPID is required to reach host netns")
	}
	if len(podSpec.Containers) != 1 {
		t.Fatal("expected one node-agent container")
	}
	container := podSpec.Containers[0]
	if container.SecurityContext == nil || container.SecurityContext.Privileged == nil || !*container.SecurityContext.Privileged {
		t.Fatal("existing node-agent privilege level is required for host netns access")
	}
	if container.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Fatal("CI-loaded images must not force a registry pull")
	}
	if len(podSpec.Volumes) != 1 || podSpec.Volumes[0].HostPath == nil || podSpec.Volumes[0].HostPath.Path != "/proc" {
		t.Fatal("host /proc mount missing")
	}
}
```

```go
func TestEnsureNodeAgentDaemonSet_CreatesWhenMissing(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	if err := ensureNodeAgentDaemonSet(ctx, clientset, "kubevpn", "test-image"); err != nil {
		t.Fatal(err)
	}
	if _, err := clientset.AppsV1().DaemonSets("kubevpn").Get(ctx, "kubevpn-nodeagent", metav1.GetOptions{}); err != nil {
		t.Fatalf("daemonset was not created: %v", err)
	}
}
```

- [ ] **Step 2: 确认失败**
Expected: FAIL — 函数不存在。

- [ ] **Step 3: 实现并接线**

DaemonSet 必须包含：
- `metadata.name=kubevpn-nodeagent`、`metadata.namespace=<manager namespace>`。
- `hostNetwork:false`、`hostPID:true`、`privileged:true`、只读 `/proc` 挂载。
- Downward API env：`NODE_NAME=spec.nodeName`、`POD_IP=status.podIP`、`MANAGER_NAMESPACE=<namespace>`、`IMAGE=<image>`。
- 容器执行 `kubevpn node-agent`，`imagePullPolicy: IfNotPresent`。
- 复用 manager 的 ServiceAccount，不创建额外 Job/RBAC。

`ensureNodeAgentDaemonSet` 先 Get；NotFound 则 Create；存在且镜像/模板不同则 Update；其他错误返回 `%w`。`createOutboundPod` 在 manager Deployment Ready 后调用它；Upgrade 路径在解析出 node-agent 模式后调用它。调用失败必须返回错误，不降级 sidecar。

- [ ] **Step 4: 验证并提交**

```bash
go test ./pkg/cluster/manager/ -run 'TestGenNodeAgentDaemonSet|TestEnsureNodeAgentDaemonSet' -v
go build ./...
go vet ./pkg/cluster/manager/...
git add pkg/cluster/manager/nodeagent_daemonset.go pkg/cluster/manager/nodeagent_daemonset_test.go pkg/cluster/manager/manager.go
git commit -m "feat: deploy one privileged node-boundary agent per node

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

### Task 6: 普通 injector 与 xDS 切换

**Files:**
- Modify: `pkg/inject/injector.go`, `pkg/inject/nodeagent.go`
- Modify: `pkg/xds/cache.go`
- Test: `pkg/inject/injector_test.go`, `pkg/xds/cache_test.go`

**Interfaces:**
- Consumes: AttachmentPublisher、DaemonSet、agentListenerPort。
- Produces: 非 Service 目标只使用 nodeAgentInjector；xDS inbound capture listener 与 UDP listener 使用 agentListenerPort；Service 目标仍为 fargate。

- [ ] **Step 1: 写失败测试**

injector 测试：非 Service + `AttachModeSidecar` 仍返回 `*nodeAgentInjector`，Service 返回 `*fargateInjector`。xDS 测试：`AgentListenerPort=20001` 的 Virtual 生成 inbound capture listener 的 `PortValue=20001`，而不是 `15006`；UDP listener 同样使用该端口。

- [ ] **Step 2: 确认失败**

Run: `go test ./pkg/inject/ ./pkg/xds/ -run 'TestNewInjector|TestAgentListenerPort' -v`
Expected: FAIL — sidecar 选择或端口未切换。

- [ ] **Step 3: 实现切换**

`NewInjector` 保留 Service 判断；非 Service 直接 `return &nodeAgentInjector{opts: opts}`。当 `opts.AttachMode == config.AttachModeSidecar` 时记录 deprecated warning，但不切换实现。

`nodeAgentInjector.Inject` 保留 PSA 检查与 `addEnvoyConfig`（Selector/IsAgent/端口/OwnerID），删除 `EnsureAgentJob`、per-pod job 等待和 hostUsers 检查；不再调用 BatchV1().Jobs。

`pkg/xds/cache.go`：
- `toInboundCaptureListener(name string, port int32)` 增加端口参数。
- Virtual 的 `AgentListenerPort != 0` 时使用它；否则 `config.PortEnvoyInbound`。
- UDP listener 的 agent 模式也使用 `AgentListenerPort`；非 agent 路径保持原 containerPort 行为。
- 同步更新所有调用点，避免签名遗漏。

- [ ] **Step 4: 验证并提交**

```bash
go test ./pkg/inject/ ./pkg/xds/ -run 'TestNewInjector|TestAgentListenerPort' -v
go build ./...
go vet ./pkg/inject/... ./pkg/xds/...
git add pkg/inject/injector.go pkg/inject/nodeagent.go pkg/xds/cache.go pkg/inject/injector_test.go pkg/xds/cache_test.go
git commit -m "feat: route ordinary proxy workloads through node-boundary agent only

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

### Task 7: 删除旧 per-pod Job 体系与测试 pin

**Files:**
- Delete: `pkg/inject/nodeagent_job.go`, `pkg/inject/nodeagent_job_test.go`
- Modify: `pkg/inject/mesh.go`, `pkg/cluster/manager/manager.go`
- Modify: `pkg/cluster/manager/resources.go`, `charts/kubevpn/templates/role.yaml`
- Modify: `pkg/handler/function_ssh_test.go`
- Delete/rewrite: 依赖旧 Job 行为的测试

**Interfaces:**
- Produces: 不再有 `EnsureAgentJob`/`GenAgentJobSpec`/`AgentJobName`/旧 reconciler 生产调用。

- [ ] **Step 1: 全局引用检查**

```bash
grep -RIn "EnsureAgentJob\|GenAgentJobSpec\|AgentJobName\|BatchV1().Jobs\|deleteAgentJobs\|cleanupAllNodeAgentJobs" pkg cmd charts
```

Expected: 列出的每处引用都必须删除或改为新附件路径，不能遗留生产调用。

- [ ] **Step 2: 删除旧实现与 RBAC**

- 删除 per-pod Job generator/ensure/等待逻辑及只服务它的测试。
- 从 `mesh.go` 移除 `deleteAgentJobs` 及其调用。
- 从 manager 移除 `cleanupAllNodeAgentJobs` 及其调用。
- 从 genRole 和 chart role 删除 batch/jobs 规则；`pods/exec` 不得存在。
- 更新 RBAC 断言到实际剩余规则数。
- 保留 sync/run/Fargate 仍需要的 sidecar 生成与清理代码。

- [ ] **Step 3: 删除普通测试的 sidecar pin**

`pkg/handler/function_ssh_test.go` 中删除 `01-connect/pin-sidecar` 和 `07-central-connect/pin-sidecar-in-kubevpn-ns` 两个 pin 子测试及其恢复逻辑。不要删除其他测试步骤。新增断言：普通 proxy 后 reviews pod 的 containers 不含 `config.ContainerSidecarVPN` 或 `config.ContainerSidecarEnvoy`。

- [ ] **Step 4: 验证并提交**

```bash
go build ./...
go vet ./pkg/...
go test ./pkg/inject/ ./pkg/cluster/manager/ ./pkg/xds/ ./pkg/handler/ -count=1
make ut-compile
git add -A
git commit -m "refactor: remove per-pod node-agent jobs and ordinary sidecar test pins

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

### Task 8: 真集群验收、文档与全量验证

**Files:**
- Create/Modify: `pkg/handler/nodeboundary_function_integration_test.go`
- Modify: `docs/55-node-hosted-agent.md`
- Modify: `docs/superpowers/specs/2026-11-25-sidecar-removal-phase1-design.md`（状态改 implemented）

**Interfaces:**
- Produces: 可复现的真集群证据，证明普通 proxy 已无 sidecar。

- [ ] **Step 1: 补齐真集群生命周期测试**

集成测试必须是多阶段故事，不是单函数调用。文件以 `//go:build integration` 开头，使用现有 `sshUt` 辅助方法；不允许新建只调用单个内部函数的单元测试：

```go
func TestIntegration_NodeBoundaryFunction(t *testing.T) {
	u := &sshUt{}
	t.Run("01-connect/init", u.init)
	t.Run("01-connect/connect", u.kubevpnConnect)

	t.Run("02-full-proxy/nonintrusive", func(t *testing.T) {
		podsBefore, err := u.clientset.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{LabelSelector: "app=reviews"})
		if err != nil || len(podsBefore.Items) == 0 {
			t.Fatalf("list reviews before attach: %v", err)
		}
		uid := podsBefore.Items[0].UID
		generation, err := u.clientset.AppsV1().Deployments("default").Get(context.Background(), "reviews", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		u.kubevpnProxy(t)
		podsAfter, err := u.clientset.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{LabelSelector: "app=reviews"})
		if err != nil || len(podsAfter.Items) == 0 {
			t.Fatalf("list reviews after attach: %v", err)
		}
		if podsAfter.Items[0].UID != uid {
			t.Fatal("attach restarted the target pod")
		}
		deployAfter, err := u.clientset.AppsV1().Deployments("default").Get(context.Background(), "reviews", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if deployAfter.Generation != generation.Generation {
			t.Fatal("attach changed the workload generation")
		}
		for _, c := range podsAfter.Items[0].Spec.Containers {
			if c.Name == pkgconfig.ContainerSidecarVPN || c.Name == pkgconfig.ContainerSidecarEnvoy {
				t.Fatalf("sidecar found in ordinary proxy pod: %s", c.Name)
			}
		}
	})
	t.Run("03-mesh/portmap/leave-reproxy", func(t *testing.T) {
		u.kubevpnProxyWithServiceMesh(t)
		u.serviceMeshReviewsServiceIP(t)
		u.kubevpnLeave(t)
		u.kubevpnProxyWithServiceMeshAndK8sServicePortMap(t)
		u.serviceMeshReviewsServiceIPPortMap(t)
	})
	t.Run("04-agent-restart", func(t *testing.T) {
		cmd := exec.Command("kubectl", "delete", "pod", "-n", "kubevpn", "-l", "app=kubevpn-nodeagent", "--wait=true")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("delete node agent: %v: %s", err, output)
		}
		u.healthCheckServiceDetails(t)
	})
	t.Run("05-target-pod-restart", func(t *testing.T) {
		cmd := exec.Command("kubectl", "delete", "pod", "-n", "default", "-l", "app=reviews", "--wait=true")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("delete target pod: %v: %s", err, output)
		}
		u.healthCheckServiceDetails(t)
	})
	t.Run("06-quit", u.kubevpnQuit)
}
```

以上测试通过后再运行完整 `TestFunctions` 与 `TestSSHFunctions`；不能在只有单一辅助检查通过时宣布阶段完成。

Linux CI 超过 40 分钟按失败处理；失败时输出 NODE_ATTACHMENTS、宿主机 PREROUTING 和 agent/Envoy 日志。

- [ ] **Step 2: 执行完整验证**

```bash
go build ./...
go vet ./pkg/...
go test ./pkg/...
make ut-compile
make ut
```

随后推送并观察 linux/macOS TestFunctions、TestSSHFunctions、image、windows、PoC 全部通过。任何失败必须按日志修复，不得重新引入 sidecar 降级。

- [ ] **Step 3: 更新文档并提交**

docs/55 记录旧 per-pod Job 架构已被 node-boundary DaemonSet 替代，包含 PoC 与真集群 run URL。第一阶段 spec 状态改 implemented。提交：

```bash
git add pkg/handler/nodeboundary_function_integration_test.go docs/55-node-hosted-agent.md docs/superpowers/specs/2026-11-25-sidecar-removal-phase1-design.md
git commit -m "docs: record phase 1 sidecar removal verification

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

## Self-Review

1. **Spec coverage:** 模型/端口（Task 1）、附件发布（Task 2）、宿主机 DNAT（Task 3）、agent 运行时（Task 4）、DaemonSet（Task 5）、injector/xDS 切换（Task 6）、旧体系清理（Task 7）、真集群验收与文档（Task 8）均有对应任务。sync/run/Fargate 不提前迁移。
2. **Placeholder scan:** 无 TBD/TODO；任务包含具体文件、接口、测试、命令与提交信息。
3. **Type consistency:** `NodeAttachment`、`AgentListenerPort`、`EnsureAgentListenerPorts`、`AttachmentPublisher`、`ConvergeHostDNAT`、`nodeagent.Run`、`RenderEnvoyBootstrapWithAdmin` 在定义与消费处一致。`toInboundCaptureListener(name, port)` 的签名变更要求更新所有调用点。
4. **风险边界:** 旧 per-pod Job 在切换阶段禁用；sidecar 不回退；hostNetwork=false；agent-source DNAT 排除；每节点单隧道；先 fake 集成测试再真集群验收。
