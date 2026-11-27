# Node-hosted Traffic Agent (Non-intrusive Proxy Attachment)

> Status: **implemented** — the node-boundary DaemonSet is the only traffic
> attachment mode for ordinary proxy/connect, run, sync, and Service targets.
> Workload sidecar injection, Fargate/SSH tunnels, and legacy attach-mode
> selection have been removed. The legacy per-pod Job architecture is historical
> context only in §14–§21.

## 1. The Question

The original node-agent proposal attached a privileged Job to every target pod and entered
that pod's network namespace. It avoided a rollout, but still created one Job and one TUN
lease per replica and coupled correctness to pod-local netns and `ORIGINAL_DST` behavior.
The production design is now a **node-boundary agent** with these properties:

- no workload spec mutation and no rolling restart;
- one DaemonSet agent per node and one tunnel/DHCP lease per node;
- no per-pod Job, per-pod TUN lease, or per-pod Envoy network namespace dependency;
- declarative desired state: the manager publishes attachments, the agent converges host
  iptables and local Envoy processes;
- `hostNetwork=false`, `hostPID=true`, read-only host `/proc`, and the existing privileged
  agent level.

There is no sidecar fallback for any workload or Service target. Legacy sidecar-injected
workloads and `hostNetwork` workloads are rejected instead of migrated.

## 2. Requirements

| # | Requirement |
|---|---|
| R1 | Zero workload modification: PodSpec and controller templates untouched, no rollout, no pod restart |
| R2 | Routing semantics identical to mesh: OwnerID rules, header splitting, multi-user merge, TUN IP hot update |
| R3 | Proxy/connect, run, sync, and Service targets use only node-boundary; no sidecar fallback |
| R4 | Declarative desired state with stable allocation and convergence, not per-pod imperative job reconciliation |
| R5 | One tunnel and one agent lease per node; no per-pod jobs or leases |
| R6 | Correct podIP **and Service ClusterIP** interception without relying on `ORIGINAL_DST` across netns |
| R7 | Attach returns only after the actual listeners are ready; uninstall leaves no agent or host DNAT state |

## 3. Mechanism

```text
ProxyInject (server-side)
  │
  ├─ write base Virtual/Rule (OwnerID, headers, ports, selector, IsAgent)
  │
  ├─ AttachmentPublisher (every 5s)
  │   ├─ allocate a stable admin base port for each workload
  │   ├─ list target pods and matching Services
  │   ├─ allocate stable direct listeners per pod/Service/original port
  │   ├─ publish NODE_ATTACHMENTS
  │   ├─ publish a derived Virtual per (namespace, workload, node)
  │   └─ persist AGENT_PORT_ALLOCATIONS tombstones
  │
  └─ node-boundary DaemonSet agent (one per node)
      ├─ one kubevpn server tunnel: OwnerID=<nodeName>, IsAgent=true
      ├─ watch the manager ConfigMap through a metadata.name-filtered informer
      ├─ filter attachments to this node
      ├─ insert marked host-netns PREROUTING rules BEFORE KUBE-SERVICES:
      │    dst podIP/ServiceIP + original dport → agentPodIP:listenerPort
      │    exclude every node-agent source IP to prevent origin-forwarding loops
      └─ one Envoy per workload on this node:
           direct listener per pod/Service/original port
           header match → user TUN IP through the node tunnel
           no match     → explicit podIP:containerPort or ClusterIP:ServicePort
```

`ORIGINAL_DST` is deliberately **not** used: the host and agent run in different network
namespaces, so the sidecar capture/restore assumption is invalid. Direct listeners and
explicit return endpoints are required. This was proven by real-cluster errors
`original_dst_load_balancer: No downstream connection or no original_dst`.

## 4. DaemonSet spec

| Field | Value | Why |
|---|---|---|
| name | `kubevpn-nodeagent` | one DaemonSet per manager namespace |
| `hostNetwork` | `false` | the agent uses its own Pod IP as the DNAT target |
| `hostPID` | `true` | required to enter the host network namespace with nsenter |
| privilege | `privileged: true` | required in the PoC environment for host netns and TUN |
| hostPath | `/proc` mounted read-only at `/host/proc` | access to `/host/proc/1/ns/net` |
| service account | traffic-manager SA | get/list/watch only the manager ConfigMap |
| pod identity | Downward API `NODE_NAME`, `POD_IP` | node filtering and DNAT target |
| TLS env | manager Secret cert/key/server/obfuscation | same manager tunnel contract |
| SA UID annotation | `kubevpn.io/service-account-uid` | forces a rollout after manager reinstall |
| imagePullPolicy | `IfNotPresent` | supports CI-loaded images |

The agent image contains both `kubevpn server` and `envoy`; each workload's Envoy gets a
unique admin port (`adminBase+5000`) and `--base-id`.

## 5. Control-plane model

- **`Virtual.agentListenerPort`** — stable per-workload admin/base-id port. It is not the
  complete data-listener allocation.
- **`Virtual.agentOrigins`** — manager-derived per-node desired state. Each origin records
  a real pod or a Service, its return IP, and direct listener mappings.
- **`NodeAttachment`** — includes `podUID`, `serviceName`, `nodeName`, `nodeID`, target
  `podIP`, optional `originIP`, and per-port `containerPort`, `originPort`,
  `agentListenerPort`.
- **`AGENT_PORT_ALLOCATIONS`** — workload/origin assignments and tombstones. Leave removes
  attachments, not allocation history, so another workload cannot immediately reuse a port
  whose old Envoy may still be exiting. Re-proxy restores the original assignment.
- **Derived UID** — `<workloadUID>.nodes.<nodeName>`, used only by xDS. Status and leave-all
  filter derived Virtuals and operate on the base workload.
- **Explicit Service identity** — `Virtual.serviceName` publishes only the requested
  ClusterIP/ServicePort and suppresses the same inferred Service on backend Virtuals.
- **Sync target** — `Rule.syncTarget` stores clone UID/selector in the base rule. Only derived
  Virtuals contain the newest Ready clone PodIP; DHCP renewal never overwrites it.

A Service is not collapsed into a pod origin. `originIP=ClusterIP` and
`originPort=ServicePort` preserve kube-proxy/IPVS load balancing. Host rules are inserted at
PREROUTING position 1 because a Service can otherwise be consumed before a podIP-only rule.

## 6. Lifecycle and readiness

| Event | Behavior |
|---|---|
| Attach | Publisher creates attachments; the injector waits for every direct listener to appear in the node agent's admin `/listeners` output before returning |
| Pod reschedules/restarts | Publisher replaces attachment state; agent converges host rules and Envoy listeners without creating a job or lease |
| Agent restarts | Marked rules are flushed and rebuilt; a new agent Pod IP replaces the old DNAT target |
| Manager restarts | ConfigMap state and allocation tombstones persist; tunnel and xDS reconnect |
| Manager uninstall/reinstall | New ServiceAccount UID annotation rolls the DaemonSet, preventing permanent `Unauthorized` from an old token |
| TLS rotation | Env changes trigger a DaemonSet spec update instead of the image/SA early return |
| Leave | Attachments and derived Virtuals are removed; allocation tombstones remain to prevent conflicts |
| Uninstall | DaemonSet is deleted; agent termination flushes all marked host DNAT rules |

## 7. RBAC & configuration

- Agent list/watch of the manager ConfigMap is restricted with
  `metadata.name=kubevpn-traffic-manager`, matching the manager Role's `resourceNames`
  constraint.
- The manager lists target pods and Services for attachment generation. No per-pod Job,
  `pods/exec`, or host CRI socket is required.
- The manager can create/update/delete the node-agent DaemonSet and read the traffic-manager
  ServiceAccount/Secret.
- The manager namespace must permit privileged pods. There is no alternate workload sidecar
  path for restricted clusters.

## 8. Limitations

| Limitation | Notes |
|---|---|
| Privileged node access | `hostPID` and privileged containers remain a significant trust requirement; there is no unprivileged Fargate or sidecar alternative |
| `ORIGINAL_DST` | Not supported across host/agent netns; direct listeners are mandatory, not optional |
| Legacy instances | Existing sidecar-injected workloads are rejected, not migrated or reset; use a clean workload |
| `hostNetwork` targets | Rejected because host-level interception cannot distinguish their destination traffic safely |
| Allocation capacity | Tombstones are retained until the manager ConfigMap is deleted, trading eventual range exhaustion for correctness during leave/re-proxy races |

## 9. Security analysis

- No new image or third-party data-plane dependency is introduced.
- The agent holds the manager's namespaced SA and TLS secret, not a cluster-admin credential.
- Host `/proc` is read-only; host mutation is limited to marked iptables rules and local
  processes.
- The DaemonSet does not use `hostNetwork`, reducing exposure to host services.
- The remaining privileged attack surface is the price of non-intrusiveness. It must not be
  hidden behind a fallback that silently changes the security model.

## 10. Integration tests (mandatory per repo rules)

Fake-clientset and process-level coverage includes:

1. stable unique admin and per-origin allocations, including removal/re-add and exhaustion;
2. publisher creates/removes attachments and derived Virtuals, skips sidecar pods, and keeps
   state identical on an identical reconcile;
3. Service discovery maps ServicePort to targetPort and publishes a Service origin per node;
4. host DNAT inserts marked rules before KUBE-SERVICES, matches exact IP/port, excludes the
   agent source, and flushes on empty attachments;
5. direct xDS listeners avoid `ORIGINAL_DST`, use explicit podIP/ClusterIP return endpoints,
   and deduplicate clusters/endpoints;
6. readiness checks reject missing agents and missing listeners, using `httptest` and fake
   pods/ConfigMaps;
7. DaemonSet security contract, SA UID rollout, TLS env refresh, and core-resource cleanup;
8. status/leave-all hide derived Virtuals; sync routes and explicit Services remain base workloads;
9. real-cluster lifecycle: attach, mesh, portmap, leave/re-proxy, agent/target/manager
   restarts, no per-pod Jobs, no workload generation or UID change.

The PoC additionally verifies ServiceIP header/origin, multiple workloads, UDP, and
`hostNetwork=false`.

## 11. Migration completion

1. **Ordinary proxy/connect:** replaced sidecar with the node-boundary DaemonSet.
2. **Sync:** replaced the nested clone VPN with declarative original-to-clone routes.
3. **Run:** removed the hidden legacy-proxy path in host and container modes.
4. **Service:** replaced Fargate/SSH Mapper with explicit Service attachments.
5. **Final:** removed workload sidecar code, attach-mode configuration, and obsolete RPCs.

## 12. Measured vs Reasoned

| Claim | Basis |
|---|---|
| host-to-agent cross-netns DNAT works for podIP and ServiceIP | **Measured** by the node-boundary PoC, including header and origin paths |
| `ORIGINAL_DST` cannot replace explicit origins | **Measured** by real-cluster `No downstream connection or no original_dst` errors |
| Early attach can create stale remote keep-alive connections | **Measured** by direct listener success alongside remote test responses; readiness gate added |
| ServiceAccount recreation breaks old agent credentials | **Measured** by repeated `Unauthorized` after manager reinstall; UID annotation added |
| Full ordinary proxy is non-intrusive | **Measured** by integration assertions on unchanged Deployment generation and pod UID |
| Full matrix is green | **Measured** — GitHub Actions run `45846188202`: Test, Coverage, Node-Boundary PoC, and both macOS jobs passed |

## 13. Related Files

| File | Purpose |
|---|---|
| `pkg/controlplane/virtual.go`, `node_attachment.go` | attachment/origin model and stable allocators |
| `pkg/xds/attachment_publisher.go`, `service_attachments.go` | desired-state publication and Service mapping |
| `pkg/xds/cache.go` | direct listener/route/cluster generation |
| `pkg/nodeagent/runtime.go`, `supervisor.go`, `iptables.go` | node tunnel, reconciliation, Envoy supervision, host DNAT |
| `pkg/cluster/manager/nodeagent_daemonset.go` | DaemonSet security and lifecycle contract |
| `pkg/inject/nodeagent_ready.go` | actual listener readiness gate |
| `pkg/daemon/action/status.go`, `pkg/xds/inject_service.go` | status/leave filtering and per-request attach mode |
| `cmd/kubevpn/cmds/legacy_proxy.go` | hidden run/sync compatibility command |

## 14. Bug Fix: Shim Process Filtering

**Problem**: The initial `FindNetnsPID` implementation selected the first process whose cgroup contained the pod UID. On containerd systems, the `containerd-shim` process runs in the host network namespace but is also in the pod's cgroup hierarchy. Since the shim has a lower PID than the pod's pause container, the agent would enter the host network namespace instead of the pod's network namespace.

**Root Cause**:
- `containerd-shim` process appears in `/proc` with cgroup path containing pod UID
- The shim runs in the host network namespace, not the pod's
- The pause container shares the pod's network namespace, but has a higher PID
- First-match logic selects the shim, traffic interception fails silently

**Fix**:
1. **Filter shim processes**: Read `/proc/<pid>/comm` and reject known shim process names:
   - `containerd-shim`
   - `conmon`
   - `containerd`
   - `dockerd`
   - `kubelet`
2. **Exclude self**: The agent shouldn't select its own process
3. **Select smallest PID**: After filtering, select the smallest PID (pause container)
4. **Explicit error**: If only shim processes are found, return an error instead of failing silently

**Verification**:
- Added comprehensive unit tests in `pkg/agent/netns_test.go`
- Enhanced e2e probe test to verify netns inode differs from agent's own netns
- Added diagnostics dump helper in `nodeagent_function_integration_test.go` to capture failure state

## 15. Bug Fix: systemd cgroup driver Pod UID Escaping

**Problem**: The agent reported "cannot find netns for pod uid <uid> under /host/proc" on minikube clusters using the systemd cgroup driver.

**Root Cause**:
- systemd cgroup driver escapes hyphens in pod UIDs to underscores in cgroup paths:
  - cgroupfs driver: `.../kubepods/pod36a197bc-6510-443d-b926-fa0a09e91efd/...`
  - systemd driver: `.../kubelet.slice/kubelet-kubepods-besteffort-pod36a197bc_6510_443d_b926_fa0a09e91efd.slice/...`
- The original `FindNetnsPID` implementation matched the raw pod UID (with hyphens) directly against the cgroup file content
- On systemd driver clusters, this match would never succeed

**Fix**:
1. **Normalize cgroup content**: Replace underscores with hyphens in the cgroup file content before matching
2. **Why this works for both drivers**:
   - cgroupfs driver paths are unaffected (no underscores to replace)
   - systemd driver paths are converted back to raw UID form for matching
3. **Added test coverage**: Test cases for systemd cgroup driver format and mixed formats

**Verification**:
- Added `TestFindNetnsPIDMatchesPodUIDInSystemdCgroupDriver` and `TestFindNetnsPIDMatchesPodUIDInBothCgroupDrivers`
- All existing tests continue to pass
- Verified on real minikube cluster with `--cgroup-driver=systemd`

## 16. Decision: Remove auto-detect and make node-agent the default

Originally, the plan was to use an "auto" attach-mode that detected cluster privileges and chose node-agent or sidecar automatically. That was implemented (PSA fast check + canary Job), but it exposed:
- Complex interaction with TestSSHFunctions and default mode behavior
- SSH jump + node-agent mesh still had a known gap
- Auto-detection logic added cognitive and test burden

After review, we decided to:
- Remove AttachModeAuto entirely
- Make node-agent the default mode
- Keep sidecar as an explicit opt-in for restricted clusters and SSH users
- Add a clean PSA label check in nodeAgentInjector that tells users to set sidecar mode

This simplifies the code, reduces test flakiness, and makes the default behavior predictable.

## 17. Important Note: Uninstall Resets Attach-Mode

Note: When you uninstall the traffic manager (using `kubevpn uninstall kubevpn`), the attach-mode configuration is deleted. When you reinstall or the manager is automatically recreated, it will default to `attachMode=node-agent`.

Impact: Tests that rely on sidecar mode (like `TestSSHFunctions`) must explicitly pin the attach-mode after any uninstall step. Workaround for tests: After any `kubevpnUninstall` step, explicitly set the attach-mode back to `sidecar` (or your desired mode) before proceeding with proxy/mesh tests.

## 18. Bug Fix: Stale Node-Agent Jobs Cause DHCP Lease Contention

**Problem**: When the traffic manager was recreated (uninstall then install, or crash recovery), stale node-agent jobs from the previous manager would reconnect to the new manager and compete for DHCP leases. This caused 3-6 minutes of mesh traffic disruption (verified in CI TestFunctions 03/05) as:
- Stale agents held leases in the agent pool 198.18.128.0/17
- New agents and user clients had to wait for lease expiration or reclamation
- Traffic routing was unstable during this period

**Root Cause**:
- Node-agent jobs are long-lived and survive manager restarts
- They automatically reconnect to the traffic manager service
- The new manager accepts all connections, including stale agents from previous sessions
- Stale agents request/hold DHCP leases that should belong to new agents

**Fix**:
1. **Explicit cleanup during manager creation**: In `createOutboundPod` (the "fresh manager" path), call `cleanupAllNodeAgentJobs` before creating the new Deployment
2. **Cleanup scope**: Deletes all jobs with the `kubevpn/pod-uid` label across all namespaces
3. **Timing**: Runs after TLS secret creation and before Deployment creation
4. **Why this path only**: When the manager already exists (attach-mode rollout), we don't want to disrupt active agent jobs that are still being used

**Verification**:
- The fix is only in the create path, not the upgrade path (preserves existing jobs when manager is updated)
- `TestCleanupTrafficManagerResources_RemovesNodeAgentJobsAllNamespaces` already verifies the cleanup function works
- CI TestFunctions 03/05 should no longer experience the 3-6 minute disruption window

## 19. 修复记录：声明式收敛修复 SSH mesh 误诊与生命周期竞态

**此前误诊**：SSH jump hosts + node-agent mesh 被误判为 known issue（建议降级 sidecar）。

**CI 取证推翻结论**（2026-11-21 run 44899084723）：
- Agent envoy access log 显式记录请求完整链路：目标 pod netns PREROUTING DNAT(:15006) → agent envoy → 匹配路由 → 用户 TUN IP → agent vpn 隧道 → manager → SSH 中继 → 用户本地，返回 200 OK
- "SSH mesh 数据面缺口"不成立。

**真实根因**：
1. **D1**：`createOutboundPod` 重建 manager 时硬编码 `attachMode=node-agent`，覆盖用户/测试预先 pin 的 `KUBEVPN_PROXY_ATTACH_MODE=sidecar`
2. **D2**：leave→re-inject 生命周期无仲裁器，`AgentReconciler` 仅校验 Job 存在性，不校验 agent pod Ready 与目标 netns 实际 DNAT 规则，造成"全绿但拦截已死"

**修复方案**（2026-11-22 声明式收敛）：
- **Reconciler 重构**：tick 30s → 5s；逐 pod 错误隔离；三级校验（Job 存在 → agent pod Ready → agent TUN lease 心跳存活：agent 每 5s 以 OwnerID=<podUID> IsAgent=true 续租，LastRenew 超 30s 即判死）；任一失败即重建 Job（数据面校验为 lease 心跳，无需 pods/exec）
- **Inject 同步就绪**：`EnsureAgentJob` Create 后等待 agent pod Running/Ready（上限 30s），超时返回错误给客户端
- **Attach-mode 继承**：`createOutboundPod` 重建前 GET 现有 deploy 读出 `KUBEVPN_PROXY_ATTACH_MODE` 传入新 spec
- **设计文档**：`docs/superpowers/specs/2026-11-22-nodeagent-dataplane-convergence-design.md`

**Verification**：
- TestSSHFunctions 07-12 不再需要降级 sidecar
- 新增集成测试覆盖 reconciler 收敛、同步就绪、attach-mode 继承场景

## 20. 修复记录：2026-11-22 第二轮架构升级（收敛数据面 + 控制通道验证）

**问题根因**（CI run 44996267829 实锤）：
- K8s 默认 Job 删除策略会 orphan 子 pod（K8s 警告 "child pods are preserved by default when jobs are deleted"）
- 僵尸 agent pod 累积后，任一 pod 的 cleanup 会全量拆除共享 DNAT 规则 → 拦截间歇性抖动
- 此前设计的 exec 实测 probe 经 5 轮审查仍不可用（镜像无 nsenter）

**修复方案（取代 §19 的三级校验设计）**：
- **原则**：netns 里的每一条状态都必须 (a) 启动即收敛、(b) 由存活 owner 周期性重申、(c) owner 退出即清除、(d) 通过 owner 的控制通道验证——绝不伸进数据面
- **A. 收敛规则**：vpn iptables 脚本按 exact rule spec 循环 `-D PREROUTING <spec>` 清空旧规则后单条重建（幂等、接管即自愈）；Go flush path 按 comment 筛选后同样用 exact spec 删除
- **B. 生命周期绑定**：`kubevpn server`（长驻进程）每 10s 重申 DNAT 规则；SIGTERM 时 flush；所有 agent Job 删除使用 Background propagation（确保子 pod 被级联删除）
- **C. 控制通道验证**：
  - agent 隧道握手时携带 `?agent=<POD_UID>` URL 参数标识身份
  - agent 每 5s 以 OwnerID=<podUID> IsAgent=true 续租 TUN lease
  - reconciler 验证改为："该 pod 的 agent lease 最近 30s 内有续租"（启动后首 2min 为 grace period）
  - **移除**：整个 exec 机制（agent_verify.go 的 nsenter/PID 文件/退出码协议）与 pods/exec RBAC（genRole + chart 同步回退）
- **D. Agent IP 池**：agent TUN IP 固定从 agent 池 198.18.128.0/17 分配

**Verification**：
- 拦截稳定性恢复：不再因僵尸 pod 导致 DNAT 规则被误删
- CI 验证通过：无 exec probe 依赖，架构更简洁

## 21. Agent 身份标识与心跳机制

**Agent 身份标识**：
- agent 与 traffic manager 建立 gRPC 隧道时，在监听地址 URL 中携带 `?agent=<POD_UID>` 参数
- 真实示例：`tun:/tcp://${TrafficManagerService}:10801?route=${CIDR4}&agent=${POD_UID}`
- traffic manager 通过该参数识别连接为 agent，并将 pod UID 绑定到 lease

**心跳机制**：
- agent 每 5s 向 traffic manager 发送 TUN lease 续租请求（OwnerID=<podUID> IsAgent=true）
- lease store 记录 LastRenew 时间戳
- reconciler 判断 agent 存活的条件：LastRenew 在最近 30s 内（启动后首 2min 为 grace period）
- 若超时未收到心跳，reconciler 删除旧 Job 并重建
