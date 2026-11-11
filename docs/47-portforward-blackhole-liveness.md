# Port-forward Black-hole & Data-plane Liveness Watchdog

## Current recovery ownership

The client defaults to **one data `connSlot` plus one independent control slot**. Default
business dispatch goes directly to slot 0 without slot-selection parsing/hashing; lifecycle,
batching and business concurrency are preserved. Heartbeat requests bypass the business queue
via a separate control queue/conn to the **same TCP forwarding endpoint 10801**, not xDS 9002.
The control conn is excluded from `tunnel.Router`, and since R2 the server answers heartbeat
echo requests **inline on the control conn itself** (`gvisorTCPHandler.handleControlConn` →
`synthesizeEchoReply`) instead of injecting them into the client's gvisor stack. There is one
registered conn per current client address in steady state. The outer SSH/port-forward TCP
transport remains shared, so the heartbeat certifies the control path end-to-end; a stall that
black-holes only the data conn (an intact outer transport with a wedged data slot) is caught by
the slot's own 180 s read deadline instead of the 15 s watchdog.

The historical watchdog/port-forward loops described below have been replaced by
one serial `transportSupervisor` in `pkg/network/transport.go`, owned by `network.Runtime`
(`pkg/network/runtime.go`) and shared by SSH and Kubernetes port-forward. Per-generation
resources live in `transport_generation.go`, endpoint construction in `transport_prepare.go`,
evidence evaluation in `transport_health.go`, and concrete kind/backoff state in
`transport_policy.go`. The health evaluator never publishes or retires a generation itself.
The 30-second unprimed and `3 * HeartbeatInterval` steady
thresholds remain (sampled every 5 s). Health is armed after TUN startup; bootstrap uses
10-second GetTunIP calls and waits for a **current-generation data heartbeat** within the
60-second total startup budget. The budget cancels failed startup, not an established session.

RentIP, IPWatcher and RouteWatcher all use a common context dialer with
`passthrough:///kubevpn-manager`; each dial resolves the active generation's xDS
endpoint. Bootstrap timeout/unavailable errors are generation-tagged recovery
signals. Runtime watcher failures do not override healthy data-plane heartbeats.
Generation-aware data and gRPC dials resolve ephemeral loopback endpoints, never a stale
fixed port. Healthy current SSH/port-forward is sticky; only failure switches paths, with
bounded backoff. Authentication/trust/configuration failures are terminal. Heartbeats carry
nonce/generation/counter with bounded outstanding state (128 / 30 s); see docs/08.

`pkg/network/monitor.go` integrates the existing vendored **netmon 1.74.1** API. Callbacks only
coalesce wakeups; the supervisor compares current interface/route state, not callback Old/New.
`TimeJumped` remains sticky until consumed. Sampling follows actual API/SSH/proxy egress and
IPv4/IPv6 default-route fallbacks, excluding this connection's TUN identity. Route lookups send
no packets to external probe targets (endpoint hostname resolution may use DNS). Relevant changes
use `ClearPending`, not `BeginGeneration`, and request fresh evidence on the current path.
Debouncing/offline backoff never extends the recovery budget indefinitely; monitor failure leaves
the heartbeat watchdog active. Offline checks read routes and interface state from one locked
monitor snapshot rather than racing callback/publication writes. No CLI, protobuf or dependency
change is required.

`Stop` rejects new workers/events, cancels the parent, retires/joins the active generation,
waits for `startupDone` and all workers (including monitor/TUN handlers), then clears DNS/state.
`generation.mu` fences dial admission/publication against retirement, and Stop joins both
watcher apply ownership and delayed dials before returning. Terminal runtime transport
errors cancel the manager; DataSession observes that cancellation outside the
supervisor and performs cleanup without self-joining.

Current source navigation: `pkg/network/address.go` owns rentIP/IPWatcher,
`pkg/network/routes.go` owns RouteWatcher and route-source repair, and
`pkg/network/transport_policy.go` holds retry/liveness constants.
`pkg/cluster/portforward/session.go` owns joinable `PortForwardSession` handles;
`pkg/cluster/portforward/forward.go` contains the `PortForwardPod` wrapper.
`pkg/tunnel/attempt.go` owns each connection's declaration/registration and joined pumps;
`pkg/tunnel/heartbeat.go` validates probe evidence. The filenames and removed helpers
inside the historical sections below are retained only as incident provenance.

### Default transport diagnostics

The supervisor records preparation failures at Warn with a local attempt number, transport kind,
preparation stage and safe error classification. Retry events report the selected next kind and
actual scheduled delay, including the offline minimum; a network-change event may wake that wait
early. Authentication/configuration failures report `transport stopped`, without promising a retry.

Before retiring a failed generation, `transport retiring` captures its heartbeat age, probe state,
cumulative send/reply counts and data/control slot activity. The snapshot is taken **before**
`BeginGeneration` clears the old evidence. `location=undetermined` deliberately avoids turning a
missing reply into an assertion about SSH, port-forward, the server, or an intermediate network hop.
A successful local write is not delivery confirmation. Error summaries use typed classifications,
not arbitrary upstream error text that could contain proxy credentials.

After a reported failure, `transport recovered` requires a nonempty validated heartbeat timestamp
for the current generation; preparation, publication and loopback TCP dialing are insufficient.
Failure summaries are limited to one per 30 seconds per supervisor, phase and transport kind.
`supervisor_failures_delta` counts all preparation/retirement failures since the supervisor's
previous emitted failure summary, including suppressed failures of other kinds or phases. The
adjacent kind/stage/reason describes the current event, not every occurrence in that aggregate.
Recovery's `failures` field is the separate outage-wide total. User cancellation does not produce a
failure/recovery event. These logs do not change watchdog thresholds or recovery decisions.
Logging remains synchronous: a permanently blocked logging backend can delay supervisor progress;
this is not a bounded-sink or shutdown guarantee.

### Slot-local rapid-failure pacing

`pkg/tunnel/slot.go` independently paces each data/control slot when dialing succeeds
but declaration, registration, reading or writing repeatedly fails rapidly. This prevents
a hot reconnect loop without adding another transport supervisor. Default delays are:

| Consecutive rapid post-dial failure | Wait before the next dial |
|---|---|
| First | Immediate |
| Second | 62.5–125 ms |
| Third | 125–250 ms |
| Fourth and later | 250–500 ms |

The intervals use jitter with a capped exponential ceiling. **Rapid age is successful
Dial return → first terminal failure of that connection attempt**, not dial-start →
cleanup-complete: dial latency, logging, cancellation, native Close and consumer joins must
not turn an immediate failure into a stable connection. A best-effort `readDL.refresh`
error after a successful write is only logged; it does not terminate the attempt or count
toward this timing. Reaching **2 s** before the first terminal failure resets the streak;
a successful registration, write or probe alone does not. This is a churn metric, not a
health verdict.

A new generation resets the streak. It does not require consuming the shared heartbeat
probe-request channel to interrupt a wait: an already-running pacing wait may add at most
**500 ms** before the new generation's dial. Context cancellation interrupts waits promptly.
A dial failure clears the post-dial streak and uses only the existing **2 s** dial-failure
backoff; the two delays are never stacked.

The existing supervisor still owns SSH/port-forward recovery, heartbeat health and terminal
authentication/trust/configuration failures. Slot pacing changes none of that ownership or
terminal-failure behavior. It bounds retry churn; it is not an end-to-end recovery-speed
measurement.

> **Historical incident and original fix:** Sections 1–5 below preserve the original
> observations, timings and port-forward-only design. Removed helpers and timestamp-only
> priming describe that implementation, not the current supervisor summarized above.

## 1. Symptom

An idle long-lived connection tunnelled through the VPN — e.g. an SSH session to another
client's TUN IP (`198.18.x`) left open overnight — is found **closed the next morning**, even
though the traffic-manager pod was **never restarted or re-rolled** during that window.

## 2. Data path recap

The entire client data plane rides on a **single** Kubernetes port-forward:

```
TUN device → 127.0.0.1:<local port> → (k8s port-forward, SPDY) → traffic-manager pod
```

In the historical topology, all four data conn-pool slots dialed `127.0.0.1:<local port>`.
The current one-data-plus-one-control topology still shares the forwarding endpoint and outer
transport. If that one port-forward session
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
always primes long before the deadline and is never falsely torn down.

**This makes the watchdog only as trustworthy as that announcement.** The paragraph above used to
conclude that the only remaining loop was a genuine persistent black-hole (network truly down), and a
gentle ~30 s retry that self-heals when the path recovers. That was wrong on both counts. If the
client fails to announce its route, the server drops every echo reply it generates and the session can
never prime while the tunnel is otherwise fine — which is what happened in the field for 20 hours (see
[51-idle-client-route-registration.md](51-idle-client-route-registration.md)). Two consequences were
addressed there:

- **The backoff was inert for exactly this case.** `portForwardHealthySession` (30 s) equals
  `livenessStartupDeadline`, so a watchdog-killed session always reaches the "healthy" duration and
  used to reset the delay to 200 ms. `nextPortForwardDelay` now also requires the session to have
  **primed**; a never-primed session backs off toward `portForwardBlackHoleMaxDelay` (30 s), and a
  later primed session clamps it straight back down. `watchLiveness` reports the verdict via
  `livenessOutcome`, carried out through `portForwardOnce`.
- **It now escalates instead of looping quietly.** After `portForwardBlackHoleThreshold` (3)
  consecutive never-primed sessions the condition is logged (throttled to every 5 minutes) with its
  duration and a pointer to `kubevpn status`.

Note each reconnect also tears down the xDS control stream sharing this port-forward session (lease
renewal, route discovery, DNS), so a tight loop actively amplifies the outage — which is why the
black-hole ceiling is much larger than `portForwardReconnectMaxDelay`.

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

### Watcher connection bounds (without tunnel teardown)

`RouteWatcher` and `IPWatcher` bound each blocking gRPC dial to **30 seconds**, then retry after
**10 seconds** while the session is active. Their first failure is visible at Warn without
`--debug`; repeated warnings are limited to once per minute per watcher, with intervening attempts
at Debug. Debug also includes dial readiness and elapsed time. Cancellation is a normal exit.

Each watch attempt resolves the current transport generation and logs its actual xDS endpoint.
The 30-second readiness deadline applies only to the blocking gRPC dial, not to the generation-bound
watch stream. Retirement cancels the old stream and fences its route/IP updates. A watcher started
between generations waits for the next endpoint rather than permanently skipping a zero local port.

IPWatcher's reconnect reassertion (`GetTunIP`) has its own **10-second** call deadline. Failure is
logged and retains the existing fallback to `WatchTunIP` unless the generation/session was canceled.
Proposal confirmation/decline/rejection RPCs each have a separate **10-second** deadline.

These deadlines do not expire the watch streams themselves or detect a black-hole that develops
after a stream has connected. They restore control to the watcher retry loop during connection
establishment, not to the whole-tunnel reconnect path; ICMP liveness remains authoritative.

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
