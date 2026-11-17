# Node-hosted Traffic Agent (Non-intrusive Proxy Attachment)

> Status: **implemented (P1-P3)** — opt-in non-intrusive proxy attachment mode
> is now available. Sidecar injection ([17-sidecar-injection.md](17-sidecar-injection.md))
> remains the default for compatibility.

## 1. The Question

`kubevpn proxy` today patches the workload's PodSpec (VPN + Envoy sidecars) and the pods
restart during rollout. Field complaints cluster around exactly this: image overrides
(#634), fighting an operator that reconciles the pod spec, Istio coexistence (#562, #568),
and workloads where a restart is expensive or disallowed.

Telepresence answers the same demand with a **node-hosted traffic agent**: the
traffic-manager creates a privileged Job on the target pod's node that attaches to the
pod's Linux namespaces *from the outside* — no workload modification, no pod restart.
Their documented trade: it requires `hostPID`, `SYS_ADMIN`/`SYS_PTRACE`/`NET_ADMIN`/`NET_RAW`,
a read-only container-runtime socket mount, a namespace permitting PSS privileged, and it
does not support user-namespaced pods (`hostUsers: false`).

Should KubeVPN build the equivalent? This document says **yes, as an opt-in mode**, and
defines it. It deliberately does NOT follow [53-ebpf-feasibility.md](53-ebpf-feasibility.md)'s
"do not own the node" reflex — that principle governs eBPF *datapath* work; a node-hosted
agent is an attachment mechanism that still reuses every routing decision KubeVPN already
makes in userspace. The node is rented (one scoped Job per target pod), not owned
(no DaemonSet, no CNI territory, nothing that outlives the session).

## 2. Requirements

| # | Requirement |
|---|---|
| R1 | Zero workload modification: PodSpec and controller templates untouched, no rollout, no pod restart |
| R2 | Routing semantics identical to mesh mode: header splitting, multi-user OwnerID rules, TUN IP hot-update via xDS |
| R3 | Opt-in at cluster level (traffic-manager Helm value) — **no CLI flag, `cmd/` stays frozen** |
| R4 | Sidecar and Fargate modes remain available; node-agent mode is additive, never the only path |
| R5 | Session-scoped: nothing persists on the node after the last rule for the session is removed |

## 3. Mechanism

Everything downstream of packet capture is reused unchanged. Only the *delivery* of the
capture stack changes — from "patch sidecars into the pod" to "run the same two processes
outside the pod, with the pod's network namespace".

```
traffic-manager ProxyInject (server-side, existing path — pkg/xds/inject_service.go)
  │
  ├─ resolve target pods (pod selector, same as today)
  ├─ addEnvoyConfig → ENVOY_CONFIG Virtual/Rule (OwnerID, headers, ports)  ← ZERO CHANGES
  │
  └─ NEW: for each target pod:
       create kubevpn-nodeagent Job in the manager namespace, pinned to the pod's node
         nodeSelector: kubernetes.io/hostname = pod.Spec.NodeName
         one Job per pod replica
       Job container (kubevpn image, two processes, both in the POD's netns):
         1. locate the target pod's netns:
              hostPID + hostPath /proc → scan for the cgroup path containing the pod UID
              (cgroupfs .../kubepods/pod<pod-uid>/...), read /proc/<pid>/ns/net
              (fallback: CRI runtime socket, read-only mount — same as Telepresence)
         2. unix.Setns(fd, CLONE_NEWNET) — pure Go, no reexec (golang.org/x/sys/unix)
         3. write iptables rules INTO pod netns PREROUTING — identical bootstrap to the
            sidecar's (pkg/inject/container.go): DNAT tcp/udp non-cluster → :15006
         4. kubevpn server (TUN tunnel) — self-allocates its TUN IP from the control
            plane over gRPC+TLS, exactly like the VPN sidecar does today
         5. envoy — xDS to the traffic manager, capture listener on :15006,
            routes by OwnerID/headers → TUN IP
```

The DNS exemption (`! -s 127.0.0.1`, cluster-CIDR exclusions) and the `:15006`
capture-listener contract are the **same bytes** as the sidecar bootstrap. Envoy bootstrap
templates (`envoy.yaml` family, docs/17 §8) are reused as-is.

## 4. Agent Job Spec

| Field | Value | Why |
|---|---|---|
| `nodeName` / `nodeSelector` | target pod's node | the netns is only reachable from its own host |
| `hostPID: true` | required | to read other containers' `/proc` and nsenter |
| capabilities | `SYS_ADMIN`, `SYS_PTRACE`, `NET_ADMIN`, `NET_RAW` (or `privileged: true`) | `setns` + iptables + TUN creation inside a foreign netns |
| `hostPath` mounts | `/proc` (read-only); optional CRI socket (read-only) | PID discovery; fallback path only |
| env | `CIDR4`, `CIDR6`, `TrafficManagerService`, TLS secret refs | same contract as sidecars (docs/17 §4) |
| labels | `kubevpn/owner-id`, `kubevpn/pod-uid` | reconcile + leave deletion keys |
| name | `kubevpn-nodeagent-<podUID>` (deterministic, ≤63 chars) | idempotent create-or-skip in reconcile |
| restartPolicy | `OnFailure` | Job survives transient crashes; pod loss handled by reconcile |

Image: the existing kubevpn traffic-manager image family. The VPN and Envoy sidecars
already ship as containers of that image with different commands — the agent Job runs
both command entrypoints as child processes of one container. **Verify at implementation
time** that one image contains both binaries (expected yes; both sidecars use
`config.Image` today).

## 5. Injector Strategy Integration

`pkg/inject` already selects an `Injector` strategy in `NewInjector`. Add a third:

| Condition | Strategy | Containers |
|---|---|---|
| Target is a K8s Service | `fargateInjector` | SSH + Envoy (sidecar) |
| attach-mode = `node-agent` | **`nodeAgentInjector`** (new) | none — launches node-hosted Jobs |
| Otherwise | `meshInjector` | VPN + Envoy (sidecar) |

`nodeAgentInjector.Inject` reuses `addEnvoyConfig`/`VirtualStore.AddRule` verbatim, then
delegates Job creation to the traffic-manager's own namespace (injection already runs
server-side inside the manager pod — docs/17 §7 — so the Job API calls are local to the
manager, no client-side path).

`envoyRuleSpec` gains nothing; `FargateMode` stays false and the xDS side sees mesh-mode
listeners (`BindToPort=false`, iptables capture). No control-plane changes.

## 6. Lifecycle

| Event | Action |
|---|---|
| Proxy inject | create-or-skip deterministic Job per target pod |
| Pod reschedules to another node | reconcile (manager pod informer over namespaces holding rules): old Job deleted, new Job created on the new node |
| Pod deleted | reconcile deletes the orphaned Job (cross-namespace ownerRef is forbidden — labels, not ownerRefs) |
| Second user proxies the same workload | Job already exists (deterministic name); only a new Rule is appended — rules merge as today (docs/17 §5) |
| Last rule removed (leave/disconnect) | `UnpatchContainer`'s "empty" branch additionally deletes the Jobs by owner-id label |
| Manager lease reaper reclaims a dead client's rules | same deletion path as leave — nothing lingers |

## 7. RBAC & Configuration

- **RBAC additions** (manager SA, both the Helm ClusterRole and the lazy namespaced Role
  where applicable): `batch/jobs` create/get/list/watch/delete in the manager namespace.
  Pod read access already exists for injection.
- **Configuration**: Helm value on the traffic-manager, e.g.
  `proxy.attachMode: sidecar | node-agent` (default `sidecar`), mirrored in the
  `genDeploySpec` code path — the chart and code must stay in sync (docs/24 §8). The mode
  travels into `ProxyInject`'s strategy selection; the client sends no mode, the manager
  decides. **No `cmd/` change.**
- Namespace requirement (documented for users): the manager namespace must permit PSS
  `privileged`. Opt-in responsibility of the cluster admin, same as Telepresence.

## 8. Limitations (recorded honestly)

| Limitation | Notes |
|---|---|
| GKE Autopilot and hard-restricted clusters | privileged Jobs rejected — mode must refuse cleanly at inject time with a clear error, not time out |
| `hostUsers: false` (user-namespaced pods) | unsupported initially — same gap as Telepresence; detect and fall back or error |
| Mesh-internal DNS | header-intercepted traffic resolves fine; mesh-only names (Istio ServiceEntry) do not resolve during attachment — accepted, matches Telepresence |
| New privileged attack surface | mitigated in §9 |
| Per-replica Jobs | one privileged Job per replica of the target workload; acceptable for dev clusters, called out for large clusters |

## 9. Security Analysis

- Image reuse: no new artifact in the supply chain — the agent Job is the already-audited
  traffic-manager image.
- Scoping: Job lives in the manager namespace, labeled by owner/pod, deleted when the
  session ends; it holds no credentials beyond the TLS secret contract sidecars already get.
- CRI socket, when used, is read-only and only the fallback path (default is `/proc` scan).
- The Job cannot outlive its rules: the reaper path deletes it even after client crashes.
- Residual risk, stated: `SYS_ADMIN` in the host PID namespace ≈ root on the node while
  the Job lives. This is the price of non-intrusiveness and the reason the mode is opt-in
  and sidecar remains the default.

## 10. Integration Tests (mandatory per repo rules)

Wire real components against `fake.NewSimpleClientset()`, structured as multi-phase stories:

1. inject → assert ENVOY_CONFIG rule written AND Job created with correct `nodeSelector`,
   deterministic name, labels (pod UID + OwnerID)
2. second user, different headers → same workload: Job NOT duplicated, two rules present
3. pod rescheduled (simulate pod update changing `spec.nodeName`) → reconcile creates the
   new Job and deletes the old one; rules untouched
4. last user leaves → Jobs deleted, ENVOY_CONFIG Virtual removed, workload never patched
5. PSA-restricted simulation (inject error injection) → clean, explanatory failure (R4)
6. crash recovery: manager restarts → reconcile restores missing Jobs from existing rules

## 11. Phasing

1. **P1 — Job plumbing**: agent Job template + creator in `pkg/cluster/manager` (or
   `pkg/inject`), nsenter bootstrap, iptables parity, TUN self-allocation verified against
   a real cluster (`-tags=integration`).
2. **P2 — Strategy + config**: `nodeAgentInjector`, Helm value + code-path sync, clean
   refusal on restricted clusters.
3. **P3 — Reconcile**: pod-node drift, orphan cleanup, crash recovery; docs for the
   kubevpn.dev user guide.

## 12. Measured vs Reasoned

| Claim | Basis |
|---|---|
| Telepresence node-agent mechanism, privileges, pros/cons | Measured (their docs, 2026-11: `/docs/howtos/agent-modes`, `/docs/concepts/architecture`) |
| Sidecar bootstrap rules to replicate | Measured (`pkg/inject/container.go`, docs/17) |
| Injection runs server-side in the manager | Measured (`pkg/xds/inject_service.go`, docs/17 §7) |
| One image contains both vpn-server and envoy binaries | **Measured** — `config.Image` contains all binaries, invoked via subcommands |
| `/proc` cgroup scan finds the pod netns | **Implemented** (`pkg/agent/netns.go`); integration test written (`pkg/inject/nodeagent_e2e_integration_test.go`); real-cluster verification pending due to test cluster unavailability |
| Managed-cluster PSS rejection behavior | **Implemented** — clean error path with user-facing message on privileged job rejection (tested in fake-cluster integration tests) |

## 13. Related Files

| File | Purpose |
|---|---|
| `pkg/inject/injector.go` | `Injector` interface + `NewInjector` factory — third strategy lands here |
| `pkg/inject/mesh.go` | mesh strategy — reference for `nodeAgentInjector` |
| `pkg/inject/container.go` | sidecar bootstrap iptables rules to replicate |
| `pkg/inject/envoy.go` / `virtual_store.go` | rule CRUD reused verbatim |
| `pkg/xds/inject_service.go` | server-side `ProxyInject` — Job creation site |
| `pkg/cluster/manager/resources.go` | resource generator patterns for the Job spec |
| `charts/kubevpn/` | Helm value `proxy.attachMode` |
| docs/17-sidecar-injection.md | the mode this complements |
