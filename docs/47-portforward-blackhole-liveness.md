# Port-forward Black-hole & Data-plane Liveness Watchdog

## 1. Symptom

An idle long-lived connection tunnelled through the VPN — e.g. an SSH session to another
client's TUN IP (`198.18.x`) left open overnight — is found **closed the next morning**, even
though the traffic-manager pod was **never restarted or re-rolled** during that window.

## 2. Data path recap

The entire client data plane rides on a **single** Kubernetes port-forward:

```
TUN device → 127.0.0.1:<local port> → (k8s port-forward, SPDY) → traffic-manager pod
```

All four conn-pool slots dial `127.0.0.1:<local port>`. If that one port-forward session
drops, the **whole tunnel** is down until it reconnects. See
[01-network-architecture.md](01-network-architecture.md) and
[24-traffic-manager-deployment.md](24-traffic-manager-deployment.md).

## 3. Root cause — the port-forward "black-holes", and the client notices too slowly

Diagnosed from root-daemon logs against an Alibaba Cloud ACK cluster:

- The k8s port-forward frequently **"connects but black-holes" (half-open)**: the local TCP
  connects and the session *looks* alive, but **no data traverses**. One observed session
  lasted `2m36s` with **zero inbound packets** for its entire life — not even the 60 s
  heartbeat echo replies came back.
- Over ~2 days: **22×** hard `lost connection to pod` + **16×** black-holes caught only by the
  xDS health check (each logged as `Health check ... context deadline exceeded` 35 s followed by
  a port-forward teardown). Around one sample both port-forwards dropped at almost exactly the
  **1-hour** mark — consistent with a konnectivity / SLB connection-lifetime cap.
- **The client detected the black-hole far too slowly.** The only detector was the xDS health
  check on a 30 s interval with a 35 s retry budget → **up to ~65 s** before it forced a
  reconnect; the per-slot read deadline (`KeepAliveTime*3` = 180 s) is slower still. The
  reconnect backoff could then add up to 15 s more, and the replacement session often
  black-holed again. Over a night these repeated multi-second-to-minute outages accumulate and
  an idle SSH is eventually reset.

The xDS health check was **not** tearing down a healthy data plane (in the sampled window the
data plane was dead too — the whole SPDY connection was black-holed). Its problem was **speed**,
not correctness.

Key gap: the client already measures data-plane liveness end-to-end via `HeartbeatStats` (ICMP
echo replies from the server gateway — see [08-heartbeat-health.md](08-heartbeat-health.md)), but
that signal was **only reported in status**, never used to trigger recovery.

This is unrelated to the ctx / conn-pool-slot design or the transport-level inter-client gvisor
stack (see [18-gvisor-network-stack.md](18-gvisor-network-stack.md)). A separate, daytime-only
cause is traffic-manager **rollout churn**: because the Deployment uses the `Recreate` strategy
(required for lease-state correctness, see [24-traffic-manager-deployment.md](24-traffic-manager-deployment.md)),
every version change (common while developing kubevpn) kills the serving pod first, producing a
"no running pod" storm that drops all active tunnels. That is a distinct issue and was not the
overnight trigger.

## 4. Fix — ICMP heartbeat is the data-plane liveness; xDS health check removed

Goal: shrink black-hole detection + recovery from ~65–80 s to ~10–20 s so an idle SSH survives, and
stop a control-plane probe from ever tearing down a healthy data tunnel.

Design principle: the ICMP echo reply is the only signal that tests the **actual data path**
end-to-end (TUN → TCP forward port → pod → gateway → back). The old xDS health check probed a
**different port** (control plane); it happened to fail together with the data plane only when the
whole SPDY connection died. So the ICMP heartbeat — not the xDS probe — is the authoritative
data-plane liveness signal and the sole port-forward reconnect trigger, and the xDS health check is
**removed** entirely (see §4.3).

### 4.1 Faster heartbeat, decoupled from deadlines (`pkg/config/config.go`, `pkg/core/tun_client.go`)

New `config.HeartbeatInterval = 5s`, used only by the client heartbeat sender. `config.KeepAliveTime`
(60 s) is unchanged and still drives every read/write deadline and TCP keepalive. Sending the
heartbeat more often only helps the server read deadline (`KeepAliveTime*3`), never hurts it, and
lets a silent tunnel be noticed in seconds.

### 4.2 Data-plane liveness watchdog (`pkg/handler/network.go`)

`portForwardOnce` starts `watchDataPlaneLiveness`, which owns the session's `cancelFunc`. A session
is **primed** once `HeartbeatStats.LastReply()` advances past the session start (proving fresh data
flowed on *this* session — the timestamp is shared across sessions, so an older one does not count).
Two independent deadlines, each measured so a healthy session is never torn down prematurely:

- **not yet primed** → reconnect after `livenessStartupDeadline` (30 s): never became ready, or
  black-holed from the start. Must exceed a healthy reconnect's time-to-prime (slot reconnect ≤
  `SlotReconnectBackoff` + RTT).
- **primed** → reconnect once the last reply is older than `livenessSteadyThreshold`
  (`3 * HeartbeatInterval` ≈ 15 s): the tunnel went silent.

`heartbeatStats` is allocated in `newNetworkManager` (never nil, one writer, no data race), so the
watchdog runs for **every** session including the first. The core loop is extracted as
`watchLiveness(...)` for testing with small timings.

**No reconnect self-loop.** After a reconnect each slot immediately (synchronously, before its
read/write loops) sends the route-registration ICMP echo (`registrationPayloads`,
`conn_slot.go`), whose reply primes `HeartbeatStats` within ~RTT — it does not wait for the 5 s
ticker. Because `livenessStartupDeadline` (30 s) ≫ that priming time, a healthy reconnected session
always primes long before the deadline and is never falsely torn down. The only remaining loop is a
genuine persistent black-hole (network truly down): a correct, gentle ~30 s retry (≈2 apiserver
calls/min) that self-heals the instant the path recovers.

### 4.3 xDS health check removed (`pkg/handler/connect_tun.go`, `network.go`, `status.go`)

`healthCheckGRPC`/`healthCheckLoop` and the whole control-plane-health status dimension are
**deleted**: `NetworkManager.controlPlaneHealthy` + `ControlPlaneHealthy()`/`setControlPlaneHealthy()`,
the `Connection.GetControlPlaneHealthy()` interface method and its `ConnectOptions`/`DataSession`
implementations, and the `controlPlaneOK` branch of `deriveConnectionStatus`. `kubevpn status` is now
derived from TUN presence + heartbeat freshness only. `CheckPodStatus` still tears down on pod
deletion (legitimate, not xDS). Rationale: the port-forward-liveness role moved to the heartbeat
watchdog, and the control-plane-serving status bit was not separately visible (`rpc.Status` has no
field; it only folded into the `unhealthy` string). Accepted trade-off: an xDS-only black-hole (data
fine) no longer forces a whole-tunnel reconnect; `RouteWatcher`/`IPWatcher` degrade (route / TUN-IP
hot-update pause, logged) until data-plane liveness or pod lifecycle triggers a reconnect.
`config.ErrXDSNotServing` and its exit-code mapping are kept (defined sentinel / exit-code contract),
now without a producer.

### 4.4 Bounded rentIP so startup can't hang (`pkg/handler/network.go`)

With the xDS probe no longer cycling a black-holed startup session, `rentIP` drops `grpc.WithBlock`
and wraps each `GetTunIP` in `rentIPCallTimeout` (10 s) with `WaitForReady`, retrying on timeout.
A black-holed first session times out and retries while the watchdog reconnects underneath, instead
of hanging forever.

Tests: `pkg/handler/network_liveness_test.go` (never-primed → reconnect, primed-then-silent →
reconnect, fresh heartbeats → no teardown, stale-pre-session reply does not prime, eager stats
allocation, not-ready exit) and `pkg/daemon/action/status_heartbeat_test.go`
(`deriveConnectionStatus` from TUN + heartbeat only).

## 5. Server / infrastructure investigation (runbook)

The client fix makes a black-hole self-heal within ~10–15 s, but the underlying instability lives
in the ACK port-forward path (konnectivity / SLB). To find and fix the root cause, run on a host
that can reach the cluster:

1. **Pod stability:** `kubectl -n <manager-ns> get pod -l app=kubevpn-traffic-manager -w` plus
   `kubectl describe` / `get events` — confirm no evict/OOM/reschedule at the outage times; check
   `kubectl top pod` for resource pressure.
2. **konnectivity:** `kubectl -n kube-system logs -l k8s-app=konnectivity-agent --tail=500` —
   ACK proxies apiserver→pod through konnectivity; look for stream resets / timeouts.
3. **~1-hour cap:** `kubectl port-forward -n <manager-ns> pod/<tm-pod> 19002:9002`, then poll the
   local port every 30 s and record when it first goes silent — verify whether it lands near a
   fixed SLB idle / lifetime limit.
4. **apiserver SLB:** check the ACK apiserver SLB idle-timeout setting.

**Transport keepalive (assessed, deliberately not applied):** the port-forward uses SPDY
(`pkg/util/portforward.go`) with **no application-layer keepalive**; the underlying TCP keepalive
is the OS default (~2 h), which is why a black-hole goes undetected at the transport level for so
long. Injecting a short TCP keepalive into the SPDY dialer is possible but (a) may not help a
black-hole caused by a middlebox silently dropping packets (keepalive probes get dropped too) and
(b) cannot be validated without a real cluster. The §4 client fix already stops the bleeding; if
step 3 confirms a keepalive-detectable idle drop, add it as a separate, cluster-verified change.

## 6. Related

- [08-heartbeat-health.md](08-heartbeat-health.md) — heartbeat / data-plane liveness signal
- [24-traffic-manager-deployment.md](24-traffic-manager-deployment.md) — single-pod, `Recreate` strategy
- [43-macos-ci-apiserver-flakiness.md](43-macos-ci-apiserver-flakiness.md) — apiserver stalls under load
- [01-network-architecture.md](01-network-architecture.md) — full data path
