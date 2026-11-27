# Node-Agent 数据面声明式收敛 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 AgentReconciler 成为 node-agent 数据面不变量（isAgent Virtual 选中的存活 pod ⇔ 生效的 DNAT+envoy）的唯一仲裁者，并修复 attach-mode 被 connect 重建覆盖的问题。

**Architecture:** 三条修复线：(1) `createOutboundPod` 重建 manager 前继承被替换 Deployment 的 `KUBEVPN_PROXY_ATTACH_MODE`（D1）；(2) `EnsureAgentJob` 同步等待 agent pod Ready 后 inject 才返回（D2 时序面）；(3) `AgentReconciler` 升级为 5s tick、逐条目错误隔离、agent pod 健康检查 + exec 实测 DNAT 规则（D2/D3 收敛面）。

**Tech Stack:** Go 1.26, client-go fake clientset（集成测试）, remotecommand SPDY exec, sigs.k8s.io/yaml。

**Spec:** `docs/superpowers/specs/2026-11-22-nodeagent-dataplane-convergence-design.md`

## Global Constraints

- 每个任务完成后运行验证循环：`go build ./...` → `go vet ./pkg/...` → `go test ./pkg/<改动包>/...`，最后一个任务追加 `make ut-compile`
- 非 `integration` 测试必须无 kubeconfig 可跑：一律 `fake.NewSimpleClientset`，绝不调用真实集群 API；绝不把 `genericclioptions.ConfigFlags` 指向环境默认 kubeconfig
- `cmd/` 冻结：现有命令参数/行为/输出不可变，不改任何 `cmd/kubevpn/cmds/*.go` 现有文件
- 日志用 `plog.G(ctx)`（alias `plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"`）；错误小写、无尾标点、`%w` 包装
- 生成文件禁改：`pkg/daemon/rpc/*.pb.go`、`pkg/syncthing/auto/gui.files.go`
- fake.Clientset 不跑 Job/Pod 控制器：agent pod 必须由测试手工创建；ResourceVersion 冲突不模拟
- 文档冲突必须同 commit 更新：本计划更新 `docs/55-node-hosted-agent.md` §16

---

### Task 1: D3 — 修正 central status 断言

**Files:**
- Modify: `pkg/handler/function_ssh_test.go:686-907`（4 个 centerCheck* 函数）

**Interfaces:**
- Consumes: 无（纯测试修正）
- Produces: 无（e2e 断言期望与实际 `kubevpn status -o json` 输出对齐：连接级 `Namespace` 是 manager 所在 ns `kubevpn`，`ProxyList[].Namespace` 仍是 workload ns `default`）

- [ ] **Step 1: 修正 centerCheckConnectStatus**

`pkg/handler/function_ssh_test.go` 中 `centerCheckConnectStatus`（约 693 行）：

```go
	expect := connStatus{List: []*connection{{
		Namespace: "kubevpn",
		Status:    "connected",
		ProxyList: nil,
	}}}
```

- [ ] **Step 2: 修正 centerCheckProxyStatus 的连接级 Namespace**

`centerCheckProxyStatus`（约 747 行）只改 connection 级字段，`ProxyList[].Namespace: "default"` 保持不动：

```go
	expect := connStatus{List: []*connection{{
		Namespace: "kubevpn",
		Status:    "connected",
		ProxyList: []*proxyItem{{
			Namespace: "default",
			Workload:  "deployments.apps/reviews",
```

- [ ] **Step 3: 修正 centerCheckProxyWithServiceMeshStatus**

`centerCheckProxyWithServiceMeshStatus`（约 809 行）同样只改 connection 级：

```go
	expect := connStatus{List: []*connection{{
		Namespace: "kubevpn",
		Status:    "connected",
		ProxyList: []*proxyItem{{
			Namespace: "default",
			Workload:  "deployments.apps/reviews",
			RuleList: []*proxyRule{{
				Headers:       map[string]string{"env": "test"},
```

- [ ] **Step 4: 修正 centerCheckProxyWithServiceMeshAndGvisorStatus**

`centerCheckProxyWithServiceMeshAndGvisorStatus`（约 878 行）同样只改 connection 级（`Workload: "services/reviews"`、`PortMap: map[int32]int32{9080: 8080, 9081: 9081}` 均不动）：

```go
	expect := connStatus{List: []*connection{{
		Namespace: "kubevpn",
		Status:    "connected",
		ProxyList: []*proxyItem{{
			Namespace: "default",
			Workload:  "services/reviews",
```

- [ ] **Step 5: 验证并提交**

Run: `go build ./... && go vet ./pkg/handler/... && go test -tags=integration -run TestNothingJustCompile ./pkg/handler/...`
Expected: 编译通过（这些是 e2e 函数，本地无集群不执行）

```bash
git add pkg/handler/function_ssh_test.go
git commit -m "test: align central status assertions with kubevpn manager namespace

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 2: §3 — createOutboundPod 继承 attach mode

**Files:**
- Modify: `pkg/cluster/manager/manager.go:46-150`（createOutboundPod + 新增两个 helper）
- Test: `pkg/cluster/manager/manager_test.go`（追加 3 个测试）

**Interfaces:**
- Consumes: `existingAttachMode(spec *v1.PodSpec) string`（upgrade.go，包内已有）、`workload.AllContainersRunning(pod)`（discovery.go 同款用法）、`genDeploySpec(namespace, image, imagePullSecretName, attachMode string)`
- Produces: `resolveAttachModeOnRecreate(ctx context.Context, clientset kubernetes.Interface, namespace string) string`；`waitForManagerPod(ctx context.Context, clientset kubernetes.Interface, namespace string, timeout time.Duration) error`；常量 `managerRecreateGrace = 10 * time.Second`

- [ ] **Step 1: 写失败测试**

`pkg/cluster/manager/manager_test.go` 追加（import 增加 `"k8s.io/apimachinery/pkg/api/errors"` 若未引入；测试文件现有 import 已含 fake/corev1/metav1/config）：

```go
func TestResolveAttachModeOnRecreate_PreservesPin(t *testing.T) {
	ctx := context.Background()
	// Simulate a manager installed with sidecar (helm values / kubevpn install pin).
	deploy := genDeploySpec("kubevpn", "img:1", "", config.AttachModeSidecar)
	clientset := fake.NewSimpleClientset(deploy)
	if got := resolveAttachModeOnRecreate(ctx, clientset, "kubevpn"); got != config.AttachModeSidecar {
		t.Fatalf("expected pinned sidecar preserved, got %q", got)
	}
}

func TestResolveAttachModeOnRecreate_MissingDeployDefaultsNodeAgent(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	if got := resolveAttachModeOnRecreate(ctx, clientset, "kubevpn"); got != config.AttachModeNodeAgent {
		t.Fatalf("expected node-agent default, got %q", got)
	}
}

func TestWaitForManagerPod_ReturnsOnceRunning(t *testing.T) {
	ctx := context.Background()
	deploy := genDeploySpec("kubevpn", "img:1", "", config.AttachModeNodeAgent)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "kubevpn-traffic-manager-x", Namespace: "kubevpn",
		Labels: map[string]string{"app": config.ConfigMapPodTrafficManager},
	}}
	running := pod.DeepCopy()
	running.Status.Phase = corev1.PodRunning
	running.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "vpn", Ready: true}}
	clientset := fake.NewSimpleClientset(deploy, pod) // pod NOT running

	// Pending pod within a short timeout must NOT report ready.
	if err := waitForManagerPod(ctx, clientset, "kubevpn", 300*time.Millisecond); err == nil {
		t.Fatal("expected timeout while pod is not running")
	}

	// Once containers are running, the wait returns immediately.
	clientset2 := fake.NewSimpleClientset(deploy, running)
	if err := waitForManagerPod(ctx, clientset2, "kubevpn", time.Second); err != nil {
		t.Fatalf("expected nil once pod is running: %v", err)
	}
}
```

注意：测试文件若缺 `"time"` import 则补充。`fake.NewSimpleClientset` 第二个 clientset 用同一个 `running` pod 时需 `DeepCopy` 已做（上面已 DeepCopy）。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./pkg/cluster/manager/ -run 'TestResolveAttachModeOnRecreate|TestWaitForManagerPod'`
Expected: FAIL — `undefined: resolveAttachModeOnRecreate` / `undefined: waitForManagerPod`

- [ ] **Step 3: 实现 helper**

`pkg/cluster/manager/manager.go` 在 `createOutboundPod` 之前新增（import 增加 `"k8s.io/apimachinery/pkg/fields"`、`"k8s.io/apimachinery/pkg/util/wait"`、`workload "github.com/wencaiwulue/kubevpn/v2/pkg/handler/workload"`，如尚未引入）：

```go
// managerRecreateGrace bounds how long createOutboundPod waits for an
// installing manager pod to become fully running before recreating everything.
const managerRecreateGrace = 10 * time.Second

// waitForManagerPod polls until a manager pod has all containers running.
// Returns nil as soon as one qualifies; error when the timeout expires first.
func waitForManagerPod(ctx context.Context, clientset kubernetes.Interface, namespace string, timeout time.Duration) error {
	label := fields.OneTermEqualSelector("app", config.ConfigMapPodTrafficManager).String()
	return wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: label})
		if err != nil {
			return false, nil // transient read error: keep waiting
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.GetDeletionTimestamp() == nil && workload.AllContainersRunning(pod) {
				return true, nil
			}
		}
		return false, nil
	})
}

// resolveAttachModeOnRecreate reads KUBEVPN_PROXY_ATTACH_MODE from the manager
// Deployment about to be replaced, so a value set before connect (helm values,
// kubevpn install) survives manager recreation. Falls back to the node-agent
// default when the deployment is gone or the env is unset.
func resolveAttachModeOnRecreate(ctx context.Context, clientset kubernetes.Interface, namespace string) string {
	deploy, err := clientset.AppsV1().Deployments(namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return config.AttachModeNodeAgent
	}
	return existingAttachMode(&deploy.Spec.Template.Spec)
}
```

- [ ] **Step 4: 接线 createOutboundPod**

`createOutboundPod` 中，把现有的 pod 缺失 → 直接重建路径改为先短暂等待，再在清理前继承 attach mode。改动点（保持其余代码不动）：

原：

```go
	defer func() {
		if err != nil {
			CleanupTrafficManagerResources(context.Background(), clientset, namespace)
		}
	}()
	CleanupTrafficManagerResources(ctx, clientset, namespace)
```

改为：

```go
	// The install's pod may still be pulling its image when connect runs
	// (DetectPodExists requires AllContainersRunning). Give it a short grace
	// period before deleting everything — recreating clobbers any pre-connect
	// customization of the manager (e.g. helm proxy.attachMode=sidecar).
	if werr := waitForManagerPod(ctx, clientset, namespace, managerRecreateGrace); werr == nil {
		_, derr := clientset.AppsV1().Deployments(namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if derr == nil {
			plog.StepDone(ctx, "Using traffic manager in namespace %q", namespace)
			return false, nil
		}
		if !apierrors.IsNotFound(derr) {
			return false, fmt.Errorf("failed to get deployment %s: %w", config.ConfigMapPodTrafficManager, derr)
		}
	}

	// Inherit the attach mode from the deployment being replaced so a pin set
	// before connect survives recreation (design doc §3).
	attachMode := resolveAttachModeOnRecreate(ctx, clientset, namespace)

	defer func() {
		if err != nil {
			CleanupTrafficManagerResources(context.Background(), clientset, namespace)
		}
	}()
	CleanupTrafficManagerResources(ctx, clientset, namespace)
```

并把第 150 行 `deploy := genDeploySpec(namespace, image, imagePullSecretName, config.AttachModeNodeAgent)` 改为：

```go
	deploy := genDeploySpec(namespace, image, imagePullSecretName, attachMode)
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./pkg/cluster/manager/ -run 'TestResolveAttachModeOnRecreate|TestWaitForManagerPod|TestCreateOutboundPod|TestManagerUpgrade' -v`
Expected: 全部 PASS

- [ ] **Step 6: 提交**

```bash
git add pkg/cluster/manager/manager.go pkg/cluster/manager/manager_test.go
git commit -m "fix: preserve manager attach mode across createOutboundPod recreation

connect clobbered a pre-connect KUBEVPN_PROXY_ATTACH_MODE (helm values /
kubevpn install pin) because DetectPodExists requires AllContainersRunning
and a still-pulling image triggered a full manager recreate with the
node-agent default. Inherit the env from the deployment being replaced and
give the installing pod a 10s grace period first.

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 3: §2 — EnsureAgentJob 同步等待 agent pod Ready

**Files:**
- Modify: `pkg/inject/nodeagent.go:115-168`（EnsureAgentJob + 新增 helper 与可调参数）
- Test: `pkg/inject/nodeagent_job_test.go`（新建）

**Interfaces:**
- Consumes: `GenAgentJobSpec(input AgentJobInput) *batchv1.Job`、`AgentJobName(uid types.UID) string`
- Produces: 包级可调参数 `ensureAgentJobReadyTimeout`（默认 30s）、`ensureAgentJobReadyPollInterval`（默认 500ms）、`ensureAgentJobTerminatingTimeout`（默认 60s）；`waitForAgentJobPodReady(ctx context.Context, cs kubernetes.Interface, namespace, jobName string) error`（Task 4/5 的 reconciler 测试依赖 job-name 标签约定：agent pod 携带 `job-name=<jobName>` 标签，batch controller 生产环境自动打）

- [ ] **Step 1: 写失败测试**

新建 `pkg/inject/nodeagent_job_test.go`（`integration` 不加 tag；全部 fake）：

```go
package inject

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func testAgentJobInput(pod *v1.Pod) AgentJobInput {
	return AgentJobInput{
		ManagerNamespace: "kubevpn",
		Namespace:        "default",
		NodeID:           "deployments.apps.reviews",
		Image:            config.Image,
		OwnerID:          "owner-1",
		Pod:              pod,
	}
}

func readyAgentPod(jobName string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-abc",
			Namespace: "kubevpn",
			Labels:    map[string]string{"job-name": jobName},
		},
		Status: v1.PodStatus{
			Phase:              v1.PodRunning,
			ContainerStatuses:  []v1.ContainerStatus{{Name: "vpn", Ready: true}, {Name: "envoy", Ready: true}},
		},
	}
}

func pendingAgentPod(jobName string) *v1.Pod {
	p := readyAgentPod(jobName)
	p.Status.Phase = v1.PodPending
	p.Status.ContainerStatuses = nil
	return p
}

func shortenTimeouts(t *testing.T, ready, terminating time.Duration) {
	t.Helper()
	oldReady, oldTerm := ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout
	ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout = ready, terminating
	t.Cleanup(func() {
		ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout = oldReady, oldTerm
	})
}

func TestEnsureAgentJob_ReturnsWhenPodReady(t *testing.T) {
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "reviews-x", Namespace: "default", UID: "uid-1"}}
	jobName := AgentJobName(pod.UID)
	job := GenAgentJobSpec(testAgentJobInput(pod))
	clientset := fake.NewSimpleClientset(job, readyAgentPod(jobName))

	if err := EnsureAgentJob(context.Background(), clientset, testAgentJobInput(pod)); err != nil {
		t.Fatalf("EnsureAgentJob: %v", err)
	}
}

func TestEnsureAgentJob_TimesOutWhenPodNeverReady(t *testing.T) {
	shortenTimeouts(t, 300*time.Millisecond, 300*time.Millisecond)
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "reviews-x", Namespace: "default", UID: "uid-2"}}
	job := GenAgentJobSpec(testAgentJobInput(pod))
	clientset := fake.NewSimpleClientset(job, pendingAgentPod(AgentJobName(pod.UID)))

	err := EnsureAgentJob(context.Background(), clientset, testAgentJobInput(pod))
	if err == nil || !strings.Contains(err.Error(), "not ready within") {
		t.Fatalf("expected not-ready timeout, got %v", err)
	}
}

func TestEnsureAgentJob_WaitsTerminatingThenRecreates(t *testing.T) {
	shortenTimeouts(t, time.Second, 2*time.Second)
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "reviews-x", Namespace: "default", UID: "uid-3"}}
	jobName := AgentJobName(pod.UID)
	now := metav1.Now()
	job := GenAgentJobSpec(testAgentJobInput(pod))
	job.DeletionTimestamp = &now // terminating
	clientset := fake.NewSimpleClientset(job)

	// Simulate the job disappearing and its replacement pod becoming ready.
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = clientset.BatchV1().Jobs("kubevpn").Delete(context.Background(), jobName, metav1.DeleteOptions{})
		_, _ = clientset.BatchV1().Jobs("kubevpn").Create(context.Background(), GenAgentJobSpec(testAgentJobInput(pod)), metav1.CreateOptions{})
		_ = clientset.CoreV1().Pods("kubevpn").Create(context.Background(), readyAgentPod(AgentJobName(pod.UID)), metav1.CreateOptions{})
	}()

	if err := EnsureAgentJob(context.Background(), clientset, testAgentJobInput(pod)); err != nil {
		t.Fatalf("EnsureAgentJob should recreate after terminating job disappears: %v", err)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./pkg/inject/ -run TestEnsureAgentJob -v`
Expected: `TestEnsureAgentJob_TimesOutWhenPodNeverReady` FAIL（当前实现不等待就绪即返回 nil）；`TestEnsureAgentJob_ReturnsWhenPodReady` 可能 PASS（现实现提前返回）

- [ ] **Step 3: 实现**

`pkg/inject/nodeagent.go` 顶部 import 增加 `"k8s.io/apimachinery/pkg/fields"`。在 `EnsureAgentJob` 之前加可调参数与 helper：

```go
// Tunable so tests can shorten the waits; production values keep injection
// synchronous but bounded.
var (
	ensureAgentJobReadyTimeout       = 30 * time.Second
	ensureAgentJobReadyPollInterval  = 500 * time.Millisecond
	ensureAgentJobTerminatingTimeout = 60 * time.Second
)

// waitForAgentJobPodReady blocks until the agent Job's pod is running with all
// containers ready, so injection returns only once the data plane (DNAT rules
// applied by the job script, envoy and tunnel payloads) is actually up.
func waitForAgentJobPodReady(ctx context.Context, cs kubernetes.Interface, namespace, jobName string) error {
	selector := fields.OneTermEqualSelector("job-name", jobName).String()
	ctxTimeout, cancel := context.WithTimeout(ctx, ensureAgentJobReadyTimeout)
	defer cancel()
	ticker := time.NewTicker(ensureAgentJobReadyPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctxTimeout.Done():
			return fmt.Errorf("agent job %q pod not ready within %s: %w", jobName, ensureAgentJobReadyTimeout, ctxTimeout.Err())
		case <-ticker.C:
			pods, err := cs.CoreV1().Pods(namespace).List(ctxTimeout, metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				return fmt.Errorf("list agent pods for job %s: %w", jobName, err)
			}
			for i := range pods.Items {
				pod := &pods.Items[i]
				if pod.GetDeletionTimestamp() != nil || pod.Status.Phase != v1.PodRunning {
					continue
				}
				if agentPodAllReady(pod) {
					return nil
				}
			}
		}
	}
}

// agentPodAllReady reports whether every container status of the pod is ready.
func agentPodAllReady(pod *v1.Pod) bool {
	if len(pod.Spec.Containers) == 0 || len(pod.Status.ContainerStatuses) < len(pod.Spec.Containers) {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if !cs.Ready {
			return false
		}
	}
	return true
}
```

`EnsureAgentJob` 改为在每个成功返回点之前等待就绪，并把 terminating 等待的硬编码 `60*time.Second` 换成 `ensureAgentJobTerminatingTimeout`：

```go
func EnsureAgentJob(ctx context.Context, cs kubernetes.Interface, input AgentJobInput) error {
	name := AgentJobName(input.Pod.UID)
	_, err := cs.BatchV1().Jobs(input.ManagerNamespace).Create(ctx, GenAgentJobSpec(input), metav1.CreateOptions{})
	if err == nil {
		return waitForAgentJobPodReady(ctx, cs, input.ManagerNamespace, name)
	}
	if !errors.IsAlreadyExists(err) {
		return err
	}

	// Job exists: check if it's terminating
	job, err := cs.BatchV1().Jobs(input.ManagerNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// Job disappeared between Create and Get: try again once
			_, err = cs.BatchV1().Jobs(input.ManagerNamespace).Create(ctx, GenAgentJobSpec(input), metav1.CreateOptions{})
			if err != nil {
				return err
			}
			return waitForAgentJobPodReady(ctx, cs, input.ManagerNamespace, name)
		}
		return err
	}

	if job.DeletionTimestamp == nil {
		// Job exists and is not terminating: wait for its pod to be ready
		return waitForAgentJobPodReady(ctx, cs, input.ManagerNamespace, name)
	}

	// Job is terminating: wait for it to disappear
	plog.G(ctx).Debugf("Agent job %q is terminating, waiting for it to disappear", name)
	ctxWait, cancel := context.WithTimeout(ctx, ensureAgentJobTerminatingTimeout)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctxWait.Done():
			return fmt.Errorf("timed out waiting for terminating agent job %q: %w", name, ctxWait.Err())
		case <-ticker.C:
			_, err := cs.BatchV1().Jobs(input.ManagerNamespace).Get(ctx, name, metav1.GetOptions{})
			if errors.IsNotFound(err) {
				// Job is gone: recreate it
				plog.G(ctx).Debugf("Agent job %q disappeared, recreating", name)
				_, err = cs.BatchV1().Jobs(input.ManagerNamespace).Create(ctx, GenAgentJobSpec(input), metav1.CreateOptions{})
				if err != nil {
					return err
				}
				return waitForAgentJobPodReady(ctx, cs, input.ManagerNamespace, name)
			}
			if err != nil {
				plog.G(ctx).Warnf("Failed to get agent job %q while waiting: %v", name, err)
			}
			// Job still exists: keep waiting
		}
	}
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./pkg/inject/ -run TestEnsureAgentJob -v && go build ./... && go vet ./pkg/inject/...`
Expected: 全部 PASS，编译/vet 干净

- [ ] **Step 5: 提交**

```bash
git add pkg/inject/nodeagent.go pkg/inject/nodeagent_job_test.go
git commit -m "fix: EnsureAgentJob waits for agent pod readiness before returning

Injection returned as soon as the Job object existed, so a leave ->
re-proxy cycle could complete while the data plane (DNAT + envoy +
tunnel in the target pod netns) did not exist yet. Injection is now
synchronous but bounded (30s ready wait).

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 4: §1a — AgentReconciler 5s tick + 逐条目隔离 + agent pod 健康检查

**Files:**
- Modify: `pkg/xds/agent_reconciler.go`（reconcileOnce 重构、interval 默认 5s、新增 findAgentPod/agentPodHealthy）
- Test: `pkg/xds/agent_reconciler_test.go`（新建）

**Interfaces:**
- Consumes: `inject.GenAgentJobSpec` / `inject.AgentJobName` / `inject.AgentJobInput`；`controlplane.Virtual`（ConfigMap `config.KeyEnvoy`，yaml 键：`schemaVersion/Namespace/Uid/Selector/Rules(ownerID,LocalTunIPv4,LocalTunIPv6,PortMap,isAgent)`，均为生产日志观察到的真实 tag）
- Produces: `reconcileOnce(ctx) error`（签名不变，语义升级：逐条目隔离 + agent pod 健康收敛）；`agentPodHealthy(pod *v1.Pod) bool`；Task 5 将在 reconcileOnce 的 `// verify (Task 5)` 标记处插入 DNAT 校验

- [ ] **Step 1: 写失败测试**

新建 `pkg/xds/agent_reconciler_test.go`：

```go
package xds

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
)

const virtualsYAML = `- schemaVersion: 2
  Namespace: default
  Uid: deployments.apps.reviews
  Selector: app=reviews
  Rules:
  - ownerID: owner-1
    LocalTunIPv4: 198.18.0.1
    LocalTunIPv6: 2001:2::1
    PortMap:
      "9080": "9080"
    isAgent: true
`

func newReconcilerTestEnv(t *testing.T, objects ...runtime.Object) (*AgentReconciler, *fake.Clientset) {
	t.Helper()
	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kubevpn"}}
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
		Data:       map[string]string{config.KeyEnvoy: virtualsYAML},
	}
	all := append([]runtime.Object{ns, cm}, objects...)
	clientset := fake.NewSimpleClientset(all...)
	r := NewAgentReconciler(clientset, "kubevpn", nil)
	return r, clientset
}

func targetPod(uid, name string, phase v1.PodPhase, created time.Time) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			UID:               "uid-" + uid,
			Labels:            map[string]string{"app": "reviews"},
			CreationTimestamp: metav1.NewTime(created),
		},
		Status: v1.PodStatus{Phase: phase},
	}
}

func agentJobFor(pod *v1.Pod) *batchv1.Job {
	return inject.GenAgentJobSpec(inject.AgentJobInput{
		ManagerNamespace: "kubevpn",
		Namespace:        pod.Namespace,
		NodeID:           "deployments.apps.reviews",
		Image:            config.Image,
		OwnerID:          "owner-1",
		Pod:              pod,
	})
}

func agentPodFor(job *batchv1.Job, phase v1.PodPhase) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Name + "-abc",
			Namespace: "kubevpn",
			Labels:    map[string]string{"job-name": job.Name},
		},
		Status: v1.PodStatus{Phase: phase},
	}
}

func getJob(t *testing.T, cs *fake.Clientset, name string) *batchv1.Job {
	t.Helper()
	job, err := cs.BatchV1().Jobs("kubevpn").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job %s: %v", name, err)
	}
	return job
}

func TestReconcileOnce_DeletesOrphanAndCreatesForNewPod(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	oldPod := targetPod("old", "reviews-old", v1.PodRunning, now)
	job := agentJobFor(oldPod)
	r, cs := newReconcilerTestEnv(t, oldPod, job)

	// Target pod is replaced: old gone, new running.
	newPod := targetPod("new", "reviews-new", v1.PodRunning, now)
	_ = cs.CoreV1().Pods("default").Delete(ctx, oldPod.Name, metav1.DeleteOptions{})
	_ = cs.CoreV1().Pods("default").Create(ctx, newPod, metav1.CreateOptions{})

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	if _, err := cs.BatchV1().Jobs("kubevpn").Get(ctx, inject.AgentJobName(oldPod.UID), metav1.GetOptions{}); err == nil {
		t.Fatal("orphan job for deleted pod should be removed")
	}
	getJob(t, cs, inject.AgentJobName(newPod.UID)) // must exist
}

func TestReconcileOnce_ToleratesStartingAgentPod(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	pod := targetPod("p", "reviews", v1.PodRunning, now)
	job := agentJobFor(pod)
	r, cs := newReconcilerTestEnv(t, pod, job)
	_ = cs.CoreV1().Pods("kubevpn").Create(ctx, agentPodFor(job, v1.PodPending), metav1.CreateOptions{})

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	getJob(t, cs, job.Name) // starting pod must NOT trigger recreate
}

func TestReconcileOnce_RecreatesJobWithDeadAgentPod(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	pod := targetPod("p", "reviews", v1.PodRunning, now)
	job := agentJobFor(pod)
	stale := job.DeepCopy()
	stale.CreationTimestamp = metav1.NewTime(now.Add(-5 * time.Minute)) // past startup grace
	r, cs := newReconcilerTestEnv(t, pod, stale)
	// no agent pod at all

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	getJob(t, cs, job.Name) // recreated
}

func TestReconcileOnce_SkipsFailingWorkloadWithoutStallingOthers(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	pod := targetPod("p", "reviews", v1.PodRunning, now)
	r, cs := newReconcilerTestEnv(t, pod)
	cs.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == "boom" {
			return true, nil, errors.New("boom")
		}
		return false, nil, nil
	})
	// Second Virtual pointing at the broken namespace.
	cm, _ := cs.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	cm.Data[config.KeyEnvoy] = virtualsYAML + `- schemaVersion: 2
  Namespace: boom
  Uid: deployments.apps.broken
  Selector: app=broken
  Rules:
  - ownerID: owner-2
    LocalTunIPv4: 198.18.0.9
    PortMap:
      "9080": "9080"
    isAgent: true
`
	_, _ = cs.CoreV1().ConfigMaps("kubevpn").Update(ctx, cm, metav1.UpdateOptions{})

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcileOnce must not abort on per-workload failure: %v", err)
	}
	getJob(t, cs, inject.AgentJobName(pod.UID)) // healthy workload still converged
}
```

（import 中补 `"errors"`。）

- [ ] **Step 2: 运行确认失败**

Run: `go test ./pkg/xds/ -run TestReconcileOnce -v`
Expected: FAIL — `TestReconcileOnce_RecreatesJobWithDeadAgentPod` 失败（现实现不检查 agent pod）；`TestReconcileOnce_SkipsFailingWorkloadWithoutStallingOthers` 失败（现实现遇错中止返回 error）

- [ ] **Step 3: 重构 reconcileOnce**

`pkg/xds/agent_reconciler.go` 整体替换 reconcileOnce 及新增两个 helper；`NewAgentReconciler` 的默认 `interval` 从 `30 * time.Second` 改为 `5 * time.Second`（`Start` 中的 fallback 同步改）：

```go
// agentPodStartupGrace: a freshly created Job may take a while to schedule its
// pod; a missing/pending pod within this window is healthy, past it the Job is
// recreated.
const agentPodStartupGrace = 2 * time.Minute

// agentPodHealthy reports whether the agent Job's pod is on track: pending
// (starting) or running counts; missing, terminating, succeeded or failed
// does not.
func agentPodHealthy(pod *v1.Pod) bool {
	if pod == nil || pod.DeletionTimestamp != nil {
		return false
	}
	switch pod.Status.Phase {
	case v1.PodPending, v1.PodRunning:
		return true
	default:
		return false
	}
}

// findAgentPod returns the agent Job's pod, if any.
func (r *AgentReconciler) findAgentPod(ctx context.Context, jobName string) (*v1.Pod, bool) {
	pods, err := r.clientset.CoreV1().Pods(r.managerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil || len(pods.Items) == 0 {
		return nil, false
	}
	return &pods.Items[0], true
}
```

reconcileOnce：

```go
func (r *AgentReconciler) reconcileOnce(ctx context.Context) error {
	virtuals, err := r.virtuals(ctx)
	if err != nil {
		return fmt.Errorf("read virtuals: %w", err)
	}

	// Collect all pods selected by any node-agent Virtual. A per-workload
	// error is logged and skipped — one bad workload must not stall
	// convergence for every other one.
	wantPods := make(map[types.UID]*v1.Pod)
	wantOwners := make(map[types.UID]string)     // pod UID -> owner ID
	wantWorkloadNS := make(map[types.UID]string) // pod UID -> workload namespace
	wantWorkloadID := make(map[types.UID]string) // pod UID -> workload node ID (Virtual.UID)

	for _, v := range virtuals {
		if v.Selector == "" {
			continue // sidecar-mode rule, skip
		}
		if len(v.Rules) == 0 {
			continue // no rules, nothing to do
		}
		pods, err := r.clientset.CoreV1().Pods(v.Namespace).List(ctx, metav1.ListOptions{LabelSelector: v.Selector})
		if err != nil {
			plog.G(ctx).Warnf("[AgentReconciler] list pods for %s/%s failed, skipping workload: %v", v.Namespace, v.UID, err)
			continue
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			wantPods[pod.UID] = pod
			wantOwners[pod.UID] = v.Rules[0].OwnerID
			wantWorkloadNS[pod.UID] = v.Namespace
			wantWorkloadID[pod.UID] = v.UID
		}
	}

	// Delete Jobs whose pod is gone or moved nodes.
	jobs, err := r.clientset.BatchV1().Jobs(r.managerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "kubevpn/pod-uid", // presence: filter only agent Jobs
	})
	if err != nil {
		return fmt.Errorf("list agent jobs: %w", err)
	}

	jobExists := make(map[string]bool)
	for i := range jobs.Items {
		job := &jobs.Items[i]
		podUID := types.UID(job.Labels["kubevpn/pod-uid"])
		if podUID == "" {
			continue
		}

		pod, stillExists := wantPods[podUID]
		if !stillExists {
			plog.G(ctx).Infof("[AgentReconciler] deleting orphan job %s for pod %s", job.Name, podUID)
			if err := r.clientset.BatchV1().Jobs(r.managerNamespace).Delete(ctx, job.Name, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
				plog.G(ctx).Warnf("[AgentReconciler] delete orphan job %s failed: %v", job.Name, err)
			}
			continue
		}

		currentNode := job.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]
		if currentNode != pod.Spec.NodeName {
			plog.G(ctx).Infof("[AgentReconciler] pod %s moved from %s to %s, recreating job %s",
				podUID, currentNode, pod.Spec.NodeName, job.Name)
			if err := r.clientset.BatchV1().Jobs(r.managerNamespace).Delete(ctx, job.Name, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
				plog.G(ctx).Warnf("[AgentReconciler] delete job %s for reschedule failed: %v", job.Name, err)
			}
			continue
		}
		jobExists[job.Name] = true
	}

	// Converge every live pod: its Job must exist with a healthy agent pod;
	// otherwise delete + recreate so the data plane comes back. (Task 5 adds
	// the DNAT verification hook here.)
	for podUID, pod := range wantPods {
		jobName := inject.AgentJobName(podUID)
		if jobExists[jobName] {
			job, err := r.clientset.BatchV1().Jobs(r.managerNamespace).Get(ctx, jobName, metav1.GetOptions{})
			if err != nil {
				if errors.IsNotFound(err) {
					jobExists[jobName] = false // raced with deletion above
				} else {
					plog.G(ctx).Warnf("[AgentReconciler] get job %s failed, skipping: %v", jobName, err)
					continue
				}
			} else if job.DeletionTimestamp != nil {
				continue // terminating; recreate on a later pass
			} else {
				agentPod, found := r.findAgentPod(ctx, jobName)
				dead := !found
				if found {
					dead = !agentPodHealthy(agentPod)
				}
				if !found && time.Since(job.CreationTimestamp.Time) <= agentPodStartupGrace {
					dead = false // pod may still be scheduling
				}
				if dead {
					plog.G(ctx).Infof("[AgentReconciler] job %s has no healthy agent pod, recreating", jobName)
					if err := r.clientset.BatchV1().Jobs(r.managerNamespace).Delete(ctx, jobName, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
						plog.G(ctx).Warnf("[AgentReconciler] delete job %s failed: %v", jobName, err)
					}
					jobExists[jobName] = false
				}
			}
		}
		if jobExists[jobName] {
			continue
		}

		plog.G(ctx).Infof("[AgentReconciler] creating missing agent job %s for pod %s/%s",
			jobName, pod.Namespace, pod.Name)

		input := inject.AgentJobInput{
			ManagerNamespace: r.managerNamespace,
			Namespace:        wantWorkloadNS[podUID],
			NodeID:           wantWorkloadID[podUID],
			Image:            config.Image,
			OwnerID:          wantOwners[podUID],
			Secret:           r.secret,
			Pod:              pod,
		}
		job := inject.GenAgentJobSpec(input)
		_, err := r.clientset.BatchV1().Jobs(r.managerNamespace).Create(ctx, job, metav1.CreateOptions{})
		if errors.IsAlreadyExists(err) {
			continue
		}
		if err != nil {
			plog.G(ctx).Warnf("[AgentReconciler] create agent job %s failed: %v", jobName, err)
			continue
		}
	}

	return nil
}
```

import 调整：`"k8s.io/apimachinery/pkg/api/errors"` 保留，`errors`（标准库）新增，`"k8s.io/api/core/v1"` 改别名（原文件无 v1 import，`corev1` 需统一——文件顶部已用 `corev1 "k8s.io/api/core/v1"`，新代码统一用 `v1` 则需改 import 别名；为最小 diff，新代码使用现有别名 `corev1`，即 `[]corev1.Pod`→`map[types.UID]*corev1.Pod`、`v1.PodRunning`→`corev1.PodRunning`、`v1.PodPending`→`corev1.PodPending`、`v1.Pod`→`corev1.Pod`）。

注意最终 `return nil`：即使个别 workload list 失败也返回 nil（仅 warn），只有 ConfigMap/jobs 读取失败才返回 error。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./pkg/xds/ -run TestReconcileOnce -v && go build ./... && go vet ./pkg/xds/...`
Expected: 全部 PASS

- [ ] **Step 5: 提交**

```bash
git add pkg/xds/agent_reconciler.go pkg/xds/agent_reconciler_test.go
git commit -m "fix: AgentReconciler converges agent pod health per workload

Reconcile tick drops to 5s, per-workload errors no longer abort the
whole pass, and a Job whose agent pod is missing/failed past the
startup grace window is recreated — the reconciler previously only
aligned Job objects and could not detect a dead data plane.

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 5: §1b — DNAT 实测校验（exec）+ RBAC + chart

**Files:**
- Create: `pkg/xds/agent_verify.go`
- Modify: `pkg/xds/agent_reconciler.go`（struct 加 `factory`/`probe`/`lastVerify` 字段；reconcileOnce 在健康检查通过后调用 `verifyDataPlane`）
- Modify: `pkg/xds/tun_config.go:56`（构造传参）
- Modify: `pkg/cluster/manager/resources.go` genRole + `charts/kubevpn/templates/role.yaml`（pods/exec）
- Test: `pkg/xds/agent_verify_test.go`（新建）

**Interfaces:**
- Consumes: Task 4 的 reconcileOnce 收敛骨架（`// (Task 5 adds the DNAT verification hook here)` 注释处）、`AgentJobName`、agent pod 的 `job-name=` 标签
- Produces: `parseDNATCount(output string) (present, known bool)`（纯函数，可测）；`AgentReconciler.probe` 可注入字段（stub 用）；RBAC 规则 `pods/exec [create,get]`（genRole + chart 同步）

- [ ] **Step 1: 写失败测试**

新建 `pkg/xds/agent_verify_test.go`：

```go
package xds

import (
	"context"
	"testing"
	"time"
)

func TestParseDNATCount(t *testing.T) {
	cases := []struct {
		output          string
		present, known  bool
	}{
		{"3\n", true, true},   // rules present
		{"0\n", false, true},  // no rules — must recreate
		{"", false, false},    // inconclusive
		{"probe-unknown\n", false, false}, // nsenter missing
	}
	for _, c := range cases {
		present, known := parseDNATCount(c.output)
		if present != c.present || known != c.known {
			t.Errorf("parseDNATCount(%q) = (%v,%v), want (%v,%v)", c.output, present, known, c.present, c.known)
		}
	}
}

func TestReconcileOnce_RecreatesWhenDNATProbeSaysMissing(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	pod := targetPod("p", "reviews", v1.PodRunning, now) // helper from agent_reconciler_test.go
	job := agentJobFor(pod)
	r, cs := newReconcilerTestEnv(t, pod, job)
	_ = cs.CoreV1().Pods("kubevpn").Create(ctx, agentPodFor(job, v1.PodRunning), metav1.CreateOptions{})
	r.probe = func(ctx context.Context, agentPodName string) (string, error) { return "0\n", nil }

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	// Old job deleted and recreated (new CreationTimestamp).
	recreated := getJob(t, cs, job.Name)
	if time.Since(recreated.CreationTimestamp.Time) > time.Minute {
		t.Fatal("expected job to be recreated after failed DNAT probe")
	}
}

func TestReconcileOnce_KeepsJobWhenDNATProbeSaysPresent(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	pod := targetPod("p", "reviews", v1.PodRunning, now)
	job := agentJobFor(pod)
	r, cs := newReconcilerTestEnv(t, pod, job)
	_ = cs.CoreV1().Pods("kubevpn").Create(ctx, agentPodFor(job, v1.PodRunning), metav1.CreateOptions{})
	r.probe = func(ctx context.Context, agentPodName string) (string, error) { return "3\n", nil }

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	kept := getJob(t, cs, job.Name)
	if time.Since(kept.CreationTimestamp.Time) > time.Minute {
		t.Fatal("healthy job should not be recreated")
	}
}
```

（import 补 `v1 "k8s.io/api/core/v1"`、`metav1`、`batchv1` 不需要——getJob/agentPodFor 已在 reconciler_test 同包。）

- [ ] **Step 2: 运行确认失败**

Run: `go test ./pkg/xds/ -run 'TestParseDNATCount|TestReconcileOnce_RecreatesWhenDNAT|TestReconcileOnce_KeepsJobWhenDNAT' -v`
Expected: FAIL — `undefined: parseDNATCount`；probe 字段不存在

- [ ] **Step 3: 实现 agent_verify.go**

新建 `pkg/xds/agent_verify.go`：

```go
package xds

import (
	"bytes"
	"context"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// verifyInterval throttles the exec-based DNAT probe: at most one probe per
// target pod per interval, so the reconcile loop stays cheap.
const verifyInterval = time.Minute

// dnatProbeCommand reads the target pod netns' NAT table from inside the agent
// container. The agent's main process (PID 1) has setns'd into the TARGET
// pod's netns, so `nsenter -t 1 -n` re-enters it — the container's own netns
// (the agent pod) does not carry the rules. `|| echo probe-unknown` keeps a
// missing nsenter (exit 127) from looking like a transport error.
const dnatProbeCommand = `nsenter -t 1 -n iptables -t nat -S 2>/dev/null | grep -c kubevpn-nodeagent || echo probe-unknown`

// parseDNATCount classifies probe output: (present, known). known=false means
// the probe could not tell — the caller must NOT churn the Job on unknown.
func parseDNATCount(output string) (present bool, known bool) {
	line := strings.TrimSpace(output)
	switch {
	case line == "probe-unknown" || line == "":
		return false, false
	case line == "0":
		return false, true
	default:
		return true, true
	}
}

// probeDNAT execs the probe command in the agent Job's vpn container. The
// rest config is derived lazily from the factory and cached.
func (r *AgentReconciler) probeDNAT(ctx context.Context, agentPodName string) (string, error) {
	if r.restConfig == nil {
		restConfig, err := r.factory.ToRESTConfig()
		if err != nil {
			return "", err
		}
		r.restConfig = restConfig
	}
	req := r.clientset.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(r.managerNamespace).Name(agentPodName).
		SubResource("exec")
	req.VersionedParams(&corev1.PodExecOptions{
		Container: "vpn",
		Command:   []string{"/bin/sh", "-c", dnatProbeCommand},
		Stdout:    true,
		Stderr:    true,
	}, runtime.NewParameterCodec(scheme.Scheme))
	exec, err := remotecommand.NewSPDYExecutor(r.restConfig, "POST", req.URL())
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err = exec.Stream(remotecommand.StreamOptions{Stdout: &out, Stderr: &out}); err != nil {
		return "", err
	}
	return out.String(), nil
}

// verifyDataPlane returns true when the target pod's netns still carries the
// kubevpn-nodeagent DNAT rules. Inconclusive results count as healthy: the
// reconciler must never churn a Job it could not actually judge.
func (r *AgentReconciler) verifyDataPlane(ctx context.Context, podUID types.UID, jobName string) bool {
	if r.lastVerify == nil {
		r.lastVerify = make(map[types.UID]time.Time)
	}
	if time.Since(r.lastVerify[podUID]) < verifyInterval {
		return true // recently verified
	}
	agentPod, found := r.findAgentPod(ctx, jobName)
	if !found {
		return true // health check handles missing pods
	}
	var (
		out string
		err error
	)
	if r.probe != nil {
		out, err = r.probe(ctx, agentPod.Name)
	} else {
		out, err = r.probeDNAT(ctx, agentPod.Name)
	}
	if err != nil {
		plog.G(ctx).Debugf("[AgentReconciler] DNAT probe for %s failed (kept): %v", jobName, err)
		return true
	}
	r.lastVerify[podUID] = time.Now()
	present, known := parseDNATCount(out)
	if !known {
		plog.G(ctx).Debugf("[AgentReconciler] DNAT probe for %s inconclusive (kept): %q", jobName, out)
		return true
	}
	if !present {
		plog.G(ctx).Infof("[AgentReconciler] DNAT rule missing for %s (probe=%q)", jobName, out)
	}
	return present
}
```

import 补：`"k8s.io/apimachinery/pkg/runtime"`、`corev1 "k8s.io/api/core/v1"`、`"k8s.io/apimachinery/pkg/types"`、`cmdutil "k8s.io/kubectl/pkg/cmd/util"`。

- [ ] **Step 4: 接线 reconciler**

`pkg/xds/agent_reconciler.go`：

struct 增加字段：

```go
type AgentReconciler struct {
	clientset        kubernetes.Interface
	managerNamespace string
	virtuals         func(ctx context.Context) ([]*controlplane.Virtual, error)
	secret           *corev1.Secret
	interval         time.Duration
	factory          cmdutil.Factory
	restConfig       *rest.Config
	// probe is injectable for tests; nil uses probeDNAT (real exec).
	probe      func(ctx context.Context, agentPodName string) (string, error)
	lastVerify map[types.UID]time.Time
}
```

`NewAgentReconciler` 签名加 factory：

```go
func NewAgentReconciler(
	clientset kubernetes.Interface,
	managerNamespace string,
	secret *corev1.Secret,
	factory cmdutil.Factory,
) *AgentReconciler {
	return &AgentReconciler{
		clientset:        clientset,
		managerNamespace: managerNamespace,
		secret:           secret,
		interval:         5 * time.Second,
		factory:          factory,
		virtuals: func(ctx context.Context) ([]*controlplane.Virtual, error) {
			// …原有实现不动…
		},
	}
}
```

（import 补 `"k8s.io/client-go/rest"`、`cmdutil "k8s.io/kubectl/pkg/cmd/util"`。）

reconcileOnce 中，Task 4 标记处（agent pod 健康检查通过、`dead` 判定之后、`jobExists[jobName]` 为 true 的分支内）插入：

```go
				if !r.verifyDataPlane(ctx, podUID, jobName) {
					plog.G(ctx).Infof("[AgentReconciler] data-plane verification failed for job %s, recreating", jobName)
					if err := r.clientset.BatchV1().Jobs(r.managerNamespace).Delete(ctx, jobName, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
						plog.G(ctx).Warnf("[AgentReconciler] delete job %s after verify failure: %v", jobName, err)
					}
					jobExists[jobName] = false
				}
```

`pkg/xds/tun_config.go:56` 构造调用补 factory 参数：

```go
		s.agentReconciler = NewAgentReconciler(clientset, namespace, secret, factory)
```

（该处作用域内若无 `factory`，从 `TunConfigServer` 的 `factory` 字段取——构造点所在函数可见哪个就用哪个；两处均持同一 factory。）同时确认 `pkg/xds/xds.go` / 其他测试中对 `NewAgentReconciler` 的调用同步更新（grep `NewAgentReconciler` 全仓库，仅 tun_config.go 一处生产调用 + 本计划测试文件）。

- [ ] **Step 5: RBAC**

`pkg/cluster/manager/resources.go` genRole 的 PolicyRule 列表追加：

```go
			{
				// AgentReconciler DNAT verification: exec into agent pods to read
				// the target pod netns' iptables (docs/55 §6). Manager-side grant
				// only; the client kubeconfig shedding (docs/46) is unaffected.
				Verbs:     []string{"create", "get"},
				APIGroups: []string{""},
				Resources: []string{"pods/exec"},
			},
```

`charts/kubevpn/templates/role.yaml` 在 `batch/jobs` 规则后追加等价 YAML：

```yaml
  - apiGroups:
      - ""
    resources:
      - pods/exec
    verbs:
      - create
      - get
```

- [ ] **Step 6: 运行测试确认通过**

Run: `go test ./pkg/xds/ -v -run 'TestParseDNATCount|TestReconcileOnce' && go test ./pkg/cluster/manager/ -run TestGenRole -v; go build ./... && go vet ./pkg/...`
Expected: 新增测试 PASS；若 genRole 有现成 RBAC 断言测试（`grep -n "genRole" pkg/cluster/manager/*_test.go` 确认），按其模式补 pods/exec 断言后一并通过

- [ ] **Step 7: 提交**

```bash
git add pkg/xds/agent_verify.go pkg/xds/agent_verify_test.go pkg/xds/agent_reconciler.go pkg/xds/tun_config.go pkg/cluster/manager/resources.go charts/kubevpn/templates/role.yaml
git commit -m "feat: reconciler verifies agent DNAT rules via exec

The reconciler now probes the target pod netns' NAT table through the
agent's vpn container (nsenter -t 1 -n, throttled to one probe per
pod per minute). Inconclusive probes never churn the job; a confirmed
missing kubevpn-nodeagent rule recreates it. Adds pods/exec to the
manager Role and chart.

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 6: docs/55 §16 更新 + 全量验证

**Files:**
- Modify: `docs/55-node-hosted-agent.md`（§16 修复记录 + Known limitation 改写）
- Modify: `docs/superpowers/specs/2026-11-22-nodeagent-dataplane-convergence-design.md`（状态 approved → implemented）

**Interfaces:**
- Consumes: Task 1-5 全部落地的代码
- Produces: 文档与实现一致（仓库规范：文档冲突必须同 commit 更新）

- [ ] **Step 1: 改写 docs/55 的误诊条目**

在 `docs/55-node-hosted-agent.md` §16（修复记录）中：
- 撤销"SSH jump hosts + node-agent mesh | known issue"条目，替换为：SSH mesh 数据面已验证可用（agent envoy → TUN → manager → SSH 中继 → 用户，2026-11-21 run 44899084723 access log 证据）；此前失败根因是 attach-mode 被 connect 重建覆盖（§3 修复）+ leave/re-inject 生命周期无仲裁（§1/§2 修复，引用设计文档路径）
- 追加一行修复记录：2026-11-22 声明式收敛（reconciler 5s tick + agent pod 健康检查 + DNAT exec 实测 + EnsureAgentJob 同步等待 + createOutboundPod attach-mode 继承）

- [ ] **Step 2: 设计文档状态翻转**

`docs/superpowers/specs/2026-11-22-nodeagent-dataplane-convergence-design.md` 头部 `**状态:** approved` 改为 `**状态:** implemented`。

- [ ] **Step 3: 全量验证循环**

```bash
go build ./...
go vet ./pkg/...
go test ./pkg/...
make ut-compile
```

Expected: 全部通过（无集群套件）；`make ut`（含 integration）留给 CI 真集群验证 TestSSHFunctions 07-12。

- [ ] **Step 4: 提交**

```bash
git add docs/55-node-hosted-agent.md docs/superpowers/specs/2026-11-22-nodeagent-dataplane-convergence-design.md
git commit -m "docs: record declarative dataplane convergence, retire SSH mesh known-limitation

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Self-Review 记录

1. **Spec 覆盖**：§1 → Task 4+5；§2 → Task 3；§3 → Task 2；§4 → Task 1（D3 断言）+ Task 2/3/4/5 的 fake 集成测试。§4 的"09 回归故事"由 Task 4/5 的 reconciler 测试 + Task 3 的 terminating-recreate 测试共同覆盖（inject→leave→re-inject 的 job 生命周期链）。无缺口。
2. **占位符扫描**：无 TBD/TODO；所有代码块完整；`NewAgentReconciler` virtuals 闭包"原有实现不动"为显式指令而非省略。
3. **类型一致性**：`resolveAttachModeOnRecreate`/`waitForManagerPod`/`waitForAgentJobPodReady`/`parseDNATCount`/`verifyDataPlane` 签名在定义与消费处一致；`AgentReconciler` 新字段在 Task 5 定义与 reconcileOnce 使用一致；Task 3 的 `job-name=` 标签约定被 Task 4 `findAgentPod` 与 Task 5 `verifyDataPlane` 复用。
