# Node-Boundary Agent Stage 0 PoC Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在不改生产接管逻辑的前提下，用最小真集群 PoC 证明 `hostNetwork=false` 的节点边界 DNAT 数据路径可行，并修正当前唯一跨平台 CI 测试断言错误。

**Architecture:** 一个 `hostNetwork=false`、`hostPID=true`、挂载只读 `/proc`、具有 `SYS_ADMIN` 和 `NET_ADMIN` 的临时 agent Pod；在宿主机 PREROUTING 按目标 pod IP 安装 DNAT，转发到 agent Pod IP 上的静态 Envoy listener。静态配置把 `env=test` 流量路由到 authors，把无 header 流量路由到原目标 pod，分别验证命中与未命中路径；UDP 用静态 Envoy UDP proxy 独立验证。

**Tech Stack:** Bash、kubectl、Envoy、iptables/nsenter、现有 bookinfo（reviews/authors/details）、alpine/socat。

**Scope:** 仅覆盖 spec 的阶段 0。PoC 通过后，另写阶段 1/2（Virtual/NodeAttachment、DaemonSet 控制器、删除 per-pod Job 体系）的实施计划。

## Global Constraints

- 不修改 workload spec，不滚动重启业务 pod。
- **不使用 hostNetwork**；不创建 per-pod Job。
- PoC 未通过则停止本路线，并把默认 attach mode 回退为 sidecar；不得继续阶段 1/2。
- `cmd/` 冻结：本计划不修改任何现有命令文件。
- 非 `integration` 测试不得访问真实集群；本 PoC 脚本只能在显式指定测试集群后运行。
- 每个提交前运行 `go build ./...` 和相关静态检查；修改 Go 测试后额外运行 `make ut-compile`。
- Linux CI 超过 40 分钟按失败处理并立即取日志。
- 错误输出必须真实记录，不能用重试掩盖失败。

---

### Task 1: 修正 quit-reset 后的 status namespace 断言

**Files:**
- Modify: `pkg/handler/function_ssh_test.go:871-890`

**Interfaces:**
- Consumes: CI run 45104676094 的实际 `kubevpn status -o json` 输出。
- Produces: 无。

- [ ] **Step 1: 修改断言**

`centerCheckProxyWithServiceMeshAndGvisorStatus` 在 quit-reset 后新建的是 default namespace 的连接；ProxyList 中的 workload namespace 也是 default。只改 connection 级字段：

```go
	expect := connStatus{List: []*connection{{
		Namespace: "default",
		Status:    "connected",
		ProxyList: []*proxyItem{{
			Namespace: "default",
			Workload:  "services/reviews",
			RuleList: []*proxyRule{{
				Headers:       map[string]string{"env": "test"},
				LocalTunIPv4:  "127.0.0.1",
				LocalTunIPv6:  "::1",
				CurrentDevice: true,
				PortMap:       map[int32]int32{9080: 8080, 9081: 9081},
			}},
		}},
	}}}
```

- [ ] **Step 2: 验证编译与静态检查**

```bash
go build ./...
go vet ./pkg/handler/...
make ut-compile
```

Expected: 全部成功。

- [ ] **Step 3: 提交**

```bash
git add pkg/handler/function_ssh_test.go
git commit -m "test: expect default namespace after central quit-reset

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

### Task 2: 编写节点边界数据路径 PoC 脚本

**Files:**
- Create: `hack/nodeboundary-poc.sh`

**Interfaces:**
- Consumes: 环境变量 `IMAGE`（必传，agent 镜像）、`KUBECONFIG`（调用方显式设置）、`WORKLOAD_NS`（默认 `default`，需已有 bookinfo reviews/authors/details）。
- Produces: 成功时输出 `NODE_BOUNDARY_POC_OK`；失败时非零退出并保留错误日志。脚本退出时删除临时 namespace。

- [ ] **Step 1: 创建完整脚本**

```bash
#!/usr/bin/env bash
set -euo pipefail

NS="${NS:-kubevpn-nodeagent-poc}"
WORKLOAD_NS="${WORKLOAD_NS:-default}"
IMAGE="${IMAGE:?IMAGE is required}"
KUBECTL="${KUBECTL:-kubectl}"
AGENT="nodeagent-poc"
CURL_POD="nodeagent-poc-curl"
UDP_POD="nodeagent-poc-udp"
SOCAT_POD="nodeagent-poc-socat"
COMMENT="kubevpn-nodeagent-poc"

log() { printf '[node-boundary-poc] %s\n' "$*"; }
cleanup() {
  local status=$?
  # Success cleans up. Failure preserves the namespace for the diagnostic
  # commands in Task 3 Step 3; DELETE_POC_NS_ON_FAILURE=1 forces cleanup.
  if [[ $status -eq 0 || "${DELETE_POC_NS_ON_FAILURE:-0}" == "1" ]]; then
    "$KUBECTL" delete namespace "$NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  else
    log "namespace $NS preserved for diagnosis"
  fi
  return "$status"
}
trap cleanup EXIT

pod_ip() {
  "$KUBECTL" -n "$WORKLOAD_NS" get pod -l "app=$1" \
    -o jsonpath='{.items[?(@.status.phase=="Running")].status.podIP}' | awk '{print $1}'
}

wait_ready() {
  "$KUBECTL" -n "$1" wait --for=condition=Ready "pod/$2" --timeout=180s >/dev/null
}

assert_body() {
  local description="$1" command="$2" expected="$3"
  local body
  body="$(eval "$command")"
  if [[ "$body" != *"$expected"* ]]; then
    printf 'FAIL %s: expected %q, got %q\n' "$description" "$expected" "$body" >&2
    return 1
  fi
  log "PASS $description: $body"
}

# Fail early when the test topology is absent.
for app in reviews authors details; do
  test -n "$(pod_ip "$app")" || { echo "missing running $app pod in $WORKLOAD_NS" >&2; exit 1; }
done

REVIEWS_IP="$(pod_ip reviews)"
AUTHORS_IP="$(pod_ip authors)"
DETAILS_IP="$(pod_ip details)"
TARGET_UID_BEFORE="$("$KUBECTL" -n "$WORKLOAD_NS" get pod -l app=reviews -o jsonpath='{.items[0].metadata.uid}')"
GENERATION_BEFORE="$("$KUBECTL" -n "$WORKLOAD_NS" get deploy reviews -o jsonpath='{.metadata.generation}')"

"$KUBECTL" create namespace "$NS" --dry-run=client -o yaml | "$KUBECTL" apply -f - >/dev/null

# hostNetwork is deliberately false. hostPID/proc is used only to reach the
# host netns for iptables; the agent has its own cluster-routable Pod IP.
"$KUBECTL" -n "$NS" apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $AGENT
spec:
  hostNetwork: false
  hostPID: true
  containers:
  - name: agent
    image: $IMAGE
    command: ["sh", "-c", "sleep 3600"]
    securityContext:
      capabilities:
        add: ["SYS_ADMIN", "NET_ADMIN"]
    volumeMounts:
    - name: host-proc
      mountPath: /host/proc
      readOnly: true
  volumes:
  - name: host-proc
    hostPath:
      path: /proc
      type: Directory
EOF
wait_ready "$NS" "$AGENT"
AGENT_IP="$("$KUBECTL" -n "$NS" get pod "$AGENT" -o jsonpath='{.status.podIP}')"
test -n "$AGENT_IP"
log "agentPodIP=$AGENT_IP reviews=$REVIEWS_IP authors=$AUTHORS_IP details=$DETAILS_IP"

render_tcp_config() {
  local file="$1" listen="$2" admin="$3" origin="$4" node="$5"
  cat >"$file" <<EOF
node:
  id: $node
  cluster: $node
admin:
  address:
    socket_address:
      address: 127.0.0.1
      port_value: $admin
static_resources:
  listeners:
  - address:
      socket_address:
        address: 0.0.0.0
        port_value: $listen
    filter_chains:
    - filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: $node
          route_config:
            virtual_hosts:
            - name: all
              domains: ["*"]
              routes:
              - match:
                  prefix: "/"
                  headers:
                  - name: env
                    exact_match: test
                route:
                  cluster: local
              - match:
                  prefix: "/"
                route:
                  cluster: origin
          http_filters:
          - name: envoy.filters.http.router
  clusters:
  - name: local
    type: STATIC
    connect_timeout: 2s
    load_assignment:
      cluster_name: local
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address:
                address: $AUTHORS_IP
                port_value: 9080
  - name: origin
    type: STATIC
    connect_timeout: 2s
    load_assignment:
      cluster_name: origin
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address:
                address: $origin
                port_value: 9080
EOF
}

render_tcp_config /tmp/poc-reviews.yaml 20001 25001 "$REVIEWS_IP" poc-reviews
render_tcp_config /tmp/poc-details.yaml 20002 25002 "$DETAILS_IP" poc-details
"$KUBECTL" -n "$NS" cp /tmp/poc-reviews.yaml "$AGENT:/tmp/poc-reviews.yaml"
"$KUBECTL" -n "$NS" cp /tmp/poc-details.yaml "$AGENT:/tmp/poc-details.yaml"
"$KUBECTL" -n "$NS" exec "$AGENT" -- sh -c \
  'nohup envoy -c /tmp/poc-reviews.yaml --service-node poc-reviews --service-cluster poc-reviews >/tmp/poc-reviews.log 2>&1 &'
"$KUBECTL" -n "$NS" exec "$AGENT" -- sh -c \
  'nohup envoy -c /tmp/poc-details.yaml --service-node poc-details --service-cluster poc-details >/tmp/poc-details.log 2>&1 &'

# The two host-side rules have exact podIP destinations and distinct agent ports.
"$KUBECTL" -n "$NS" exec "$AGENT" -- nsenter --net=/host/proc/1/ns/net \
  iptables -t nat -A PREROUTING -p tcp -d "$REVIEWS_IP" \
  -j DNAT --to "$AGENT_IP:20001" -m comment --comment "$COMMENT"
"$KUBECTL" -n "$NS" exec "$AGENT" -- nsenter --net=/host/proc/1/ns/net \
  iptables -t nat -A PREROUTING -p tcp -d "$DETAILS_IP" \
  -j DNAT --to "$AGENT_IP:20002" -m comment --comment "$COMMENT"

"$KUBECTL" -n "$NS" run "$CURL_POD" --image=curlimages/curl:8.10.1 --restart=Never \
  --command -- sh -c 'sleep 3600' >/dev/null
wait_ready "$NS" "$CURL_POD"

assert_body "header match goes to authors" \
  "$KUBECTL -n $NS exec $CURL_POD -- curl -fsS --max-time 15 -H 'env: test' http://$REVIEWS_IP:9080/health" \
  "Authors is healthy"
assert_body "no header goes to original reviews" \
  "$KUBECTL -n $NS exec $CURL_POD -- curl -fsS --max-time 15 http://$REVIEWS_IP:9080/health" \
  "Reviews is healthy"
assert_body "second workload header match" \
  "$KUBECTL -n $NS exec $CURL_POD -- curl -fsS --max-time 15 -H 'env: test' http://$DETAILS_IP:9080/health" \
  "Authors is healthy"
assert_body "second workload origin" \
  "$KUBECTL -n $NS exec $CURL_POD -- curl -fsS --max-time 15 http://$DETAILS_IP:9080/health" \
  "Details is healthy"

# UDP uses a separate static UDP proxy and UDP echo backend; TCP success is not
# treated as evidence for UDP.
"$KUBECTL" -n "$NS" run "$UDP_POD" --image=alpine/socat:latest --restart=Never -- \
  'UDP4-RECVFROM:9081,fork EXEC:/bin/cat' >/dev/null
wait_ready "$NS" "$UDP_POD"
UDP_IP="$("$KUBECTL" -n "$NS" get pod "$UDP_POD" -o jsonpath='{.status.podIP}')"
cat >/tmp/poc-udp.yaml <<EOF
node:
  id: poc-udp
  cluster: poc-udp
admin:
  address:
    socket_address:
      address: 127.0.0.1
      port_value: 25003
static_resources:
  listeners:
  - address:
      socket_address:
        address: 0.0.0.0
        port_value: 20003
        protocol: UDP
    listener_filters:
    - name: envoy.filters.udp_listener.udp_proxy
      typed_config:
        "@type": type.googleapis.com/envoy.extensions.filters.udp.udp_proxy.v3.UdpProxyConfig
        stat_prefix: poc_udp
        cluster: udp_backend
  clusters:
  - name: udp_backend
    type: STATIC
    connect_timeout: 2s
    load_assignment:
      cluster_name: udp_backend
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address:
                address: $UDP_IP
                port_value: 9081
EOF
"$KUBECTL" -n "$NS" cp /tmp/poc-udp.yaml "$AGENT:/tmp/poc-udp.yaml"
"$KUBECTL" -n "$NS" exec "$AGENT" -- sh -c \
  'nohup envoy -c /tmp/poc-udp.yaml --service-node poc-udp --service-cluster poc-udp >/tmp/poc-udp.log 2>&1 &'
"$KUBECTL" -n "$NS" exec "$AGENT" -- nsenter --net=/host/proc/1/ns/net \
  iptables -t nat -A PREROUTING -p udp -d "$UDP_IP" \
  -j DNAT --to "$AGENT_IP:20003" -m comment --comment "$COMMENT"

"$KUBECTL" -n "$NS" run "$SOCAT_POD" --image=alpine/socat:latest --restart=Never \
  --command -- sh -c 'sleep 3600' >/dev/null
wait_ready "$NS" "$SOCAT_POD"
UDP_REPLY="$("$KUBECTL" -n "$NS" exec "$SOCAT_POD" -- sh -c \
  "echo poc-udp | socat -T 10 - UDP4:$UDP_IP:9081")"
test "$UDP_REPLY" = "poc-udp"
log "PASS UDP original destination path: $UDP_REPLY"

TARGET_UID_AFTER="$("$KUBECTL" -n "$WORKLOAD_NS" get pod -l app=reviews -o jsonpath='{.items[0].metadata.uid}')"
GENERATION_AFTER="$("$KUBECTL" -n "$WORKLOAD_NS" get deploy reviews -o jsonpath='{.metadata.generation}')"
test "$TARGET_UID_BEFORE" = "$TARGET_UID_AFTER"
test "$GENERATION_BEFORE" = "$GENERATION_AFTER"
test "$("$KUBECTL" -n "$NS" get pod "$AGENT" -o jsonpath='{.spec.hostNetwork}')" = "false"
log "PASS no workload restart, no generation change, hostNetwork=false"

echo NODE_BOUNDARY_POC_OK
```

- [ ] **Step 2: 做本地静态检查**

```bash
chmod +x hack/nodeboundary-poc.sh
bash -n hack/nodeboundary-poc.sh
```

Expected: 无语法错误。此步骤不得连接集群。

- [ ] **Step 3: 提交**

```bash
git add hack/nodeboundary-poc.sh
git commit -m "test: add node-boundary datapath PoC harness

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

### Task 3: 在指定测试集群执行 PoC 并记录结果

**Files:**
- Create: `docs/superpowers/poc/2026-11-23-node-boundary-result.md`

**Interfaces:**
- Consumes: Task 2 的 `hack/nodeboundary-poc.sh`；已发布的当前提交镜像；测试集群 `/data/.kube/config` 的 ACK context `kubernetes-admin-cf2096c3e67fb41a0b0cfc3de9a72f027`。
- Produces: 明确的 go/no-go 决策。

- [ ] **Step 1: 确认测试集群与镜像**

```bash
git push origin feat/agent-access-and-nodeagent
export KUBECONFIG=/data/.kube/config
kubectl config use-context kubernetes-admin-cf2096c3e67fb41a0b0cfc3de9a72f027
for app in reviews authors details; do kubectl get pods -l "app=$app" -n default; done
export IMAGE="ghcr.io/kubenetworks/kubevpn:$(git rev-parse HEAD)"
docker manifest inspect "$IMAGE" >/dev/null
```

Expected: bookinfo 三个 workload 有 Running pod；镜像存在。若当前提交镜像尚未发布，等待 CI image job，不得用旧镜像冒充当前实现。

- [ ] **Step 2: 执行 PoC**

```bash
export KUBECONFIG=/data/.kube/config
export IMAGE="ghcr.io/kubenetworks/kubevpn:$(git rev-parse HEAD)"
hack/nodeboundary-poc.sh 2>&1 | tee /tmp/node-boundary-poc.log
```

Expected 最后一行: `NODE_BOUNDARY_POC_OK`。

- [ ] **Step 3: 若失败，收集最小诊断并停止**

仅在失败时执行：

```bash
export KUBECONFIG=/data/.kube/config
kubectl -n kubevpn-nodeagent-poc logs nodeagent-poc --tail=200 || true
kubectl -n kubevpn-nodeagent-poc exec nodeagent-poc -- \
  nsenter --net=/host/proc/1/ns/net iptables -t nat -S PREROUTING || true
```

把失败输出原文保存到结果文档，**不得重试到偶然通过后宣称成功**。停止后续阶段，并向用户报告是否按 spec 回退默认 sidecar。

- [ ] **Step 4: 记录结果并提交**

结果文件内容：

```markdown
# Node-Boundary Agent PoC Result — 2026-11-23

- Cluster: ACK `kubernetes-admin-cf2096c3e67fb41a0b0cfc3de9a72f027`
- Image: <实际完整镜像 tag>
- hostNetwork: false
- TCP header match: <PASS/FAIL 与原始输出>
- TCP origin forwarding: <PASS/FAIL 与原始输出>
- Second workload isolation: <PASS/FAIL 与原始输出>
- UDP path: <PASS/FAIL 与原始输出>
- Workload UID/generation unchanged: <PASS/FAIL>
- Decision: <go — write stage 1 plan / no-go — revert default to sidecar>
```

用实际值替换尖括号，不留空项，然后：

```bash
git add docs/superpowers/poc/2026-11-23-node-boundary-result.md
git commit -m "docs: record node-boundary datapath PoC result

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

## Self-Review

1. **Spec coverage:** 本计划只覆盖 spec §6 阶段 0 与当前 CI 断言修正；阶段 1/2 明确留到 PoC 通过后单独计划，避免未验证架构就展开实现。UDP、非侵入、多 workload、hostNetwork=false、失败回退均有任务。
2. **Placeholder scan:** 无 TBD/TODO；脚本、命令、测试预期完整。结果文档中的尖括号要求在执行时替换为实际证据。
3. **Type consistency:** `agentListenerPort` 等阶段 1 类型没有提前引用；本计划只使用 shell/Envoy 配置，无跨任务 Go 类型依赖。
