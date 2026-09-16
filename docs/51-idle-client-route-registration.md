# Idle-Client Route Registration and the 20-Hour Reconnect Loop

## 1. Overview

A single silent `return nil` on the client took a tunnel down for 20 hours and, through four
downstream layers, ended up corrupting a lease record in the traffic manager's ConfigMap. This
document records the causal chain, because every link in it was individually reasonable and only the
combination was fatal — and because four of the five layers were invisible at the default log level.

Symptoms as first reported:

1. `TUN_ALLOCS` in the ConfigMap held a record whose `lastRenew` was 1h54m old for a client that was
   up at that moment. "Doesn't the server reclaim old IPs? Hasn't it expired?"
2. `root_daemon.log` was full of errors.

They were the same incident. Symptom 1 is a consequence of symptom 2.

## 2. The Causal Chain

```
registrationPayloads() silently returns nil            (pkg/core/tun_client.go)
  └─> data slots announce nothing on (re)connect       (pkg/core/conn_slot.go)
       └─> server RouteHub has no route for the client (pkg/core/route.go)
            └─> every heartbeat echo reply is dropped  (gvisor_tun_endpoint.go)
                 └─> liveness watchdog never primes    (pkg/handler/network.go)
                      └─> port-forward torn down every 30s, forever
                           └─> xDS stream never lives 100s
                                └─> lease renewal never persisted
                                     └─> TUN_ALLOCS.lastRenew frozen
                                          └─> a restart would release a LIVE client's IP
```

### 2.1 Why the announcement stopped

`registrationPayloads` re-derived the client's own TUN address on **every slot reconnect** by
scanning the whole OS interface table (`GetTunDeviceByConn` → `GetTunDevice`). `GetTunDevice` returned
an error as soon as *any* interface's `Addrs()` failed — including interfaces having nothing to do
with the TUN. macOS brings `awdl0`/`llw0` (AirDrop/AWDL) up and down dynamically, so the scan
succeeded at startup and failed on every reconnect afterwards. The caller ranged over a nil slice.

`heartbeats()` used the same lookup but called it **once** at goroutine start and cached the result,
which is why heartbeats kept arriving for 20 hours while registrations never did. That asymmetry is
what made the diagnosis confusing: the tunnel was demonstrably carrying client→server traffic.

### 2.2 Why a missing route is fatal for an *idle* client

Heartbeat ICMP goes out on the dedicated control conn, which the server deliberately does **not**
register in `RouteHub` (`handleControlConn`): the echo must come back over a *data* conn, because
that is what proves the data path works. See [08-heartbeat-health.md](08-heartbeat-health.md).

So the only things that can create a route for a client are a data slot's registration packet or
real outbound user traffic. An idle client with broken registration is invisible to the server: it
generates every echo reply and then drops it for want of a route.

This is a correct design, not a bug. It is precisely *because* the round trip must traverse a data
conn that a broken announcement is fatal rather than cosmetic.

### 2.3 Field evidence

One hour of traffic-manager log, one macOS client (`198.18.0.5`) plus one healthy client
(`198.18.0.1`):

| Observation | Count |
|---|---|
| Connections accepted / closed | 657 / 657 |
| `Control conn detected` | 120 |
| `[Route] Add pool conn: 198.18.0.1` (healthy client) | 53 |
| `[Route] Add pool conn: 198.18.0.5` | **0** |
| `[Route] Add route: 198.18.0.5` | 1 (from an incidental DNS query, not a registration) |
| `No route for stack output -> 198.18.0.5` | **731** (+765 for `2001:2::5`) |

A single reconnect round, in order: five conns close, five reconnect, only the control conn speaks,
the server answers the heartbeat, and the answer is dropped. The four data conns then say nothing at
all for 30 s until the next teardown.

Client side over 20 hours: 2221 × `Data plane never came up (no heartbeat echo reply within 30s)`,
at a metronomic 30.3 s interval, in a 5.7 MB log containing four distinct messages.

The `connection refused` lines in that log are **not** a separate fault: 11165 of them over 2221
rounds is exactly one per slot per round, at the instant the port-forward is cancelled, and the slot
reconnects after `SlotReconnectBackoff`.

## 3. Fixes

### 3.1 Do not re-derive what we already resolved (`pkg/core`)

`tunHandler.Handle` already resolves the interface once, successfully. That name is carried on
`tunDevice` and every address lookup goes through one seam, `tunDevice.addrs()`, which resolves **by
name** (`net.InterfaceByName`) and therefore cannot be broken by an unrelated NIC. Addresses are
still re-read per call, never cached, so `ChangeTunIP` is followed
(see [09-tun-ip-hot-update.md](09-tun-ip-hot-update.md)).

`heartbeats()` no longer fails fast when the device cannot be resolved: disabling liveness for the
rest of the session on one transient failure is the same trap in a different place.

### 3.2 One bad interface must not hide the TUN (`pkg/util/netutil`)

`GetTunDevice` skips an interface whose `Addrs()` fails and keeps looking, naming the skipped
interfaces in the not-found error. The blast radius was wider than route announcement: the same
function backs "is my TUN up?" in `daemon/action/status.go`, the route handler and sshdaemon, so one
unreadable NIC could make a healthy connection report as down.

### 3.3 Make the failure paths audible (`pkg/log`, `pkg/core`)

Six failure paths were Debug-level or entirely silent. They now warn through `log.Throttle` — one
message per key per 30 s, keyed by call site or by peer address so one noisy destination cannot mask
another:

- `registrationPayloads` producing nothing, and the announcement write failing
- heartbeat not sent (no address), control slot absent, control slot queue full
- server dropping stack output for want of a route, and dropping inter-client traffic for an
  unannounced peer

The last two are the server's only account of "I answered the client and threw the answer away".
Diagnosing this incident required switching the traffic manager to debug by hand.

### 3.4 Let the watchdog converge (`pkg/handler`)

`livenessStartupDeadline` and `portForwardHealthySession` are both 30 s, so a session the watchdog
kills for never coming up has by definition lasted long enough to be classified "healthy" — which
reset the reconnect backoff to 200 ms. The backoff was therefore disabled for the one case it
existed for.

Duration is no longer taken as evidence of health. `watchLiveness` reports whether the session ever
produced a heartbeat echo reply of its own (`livenessOutcome`, carried out through
`portForwardOnce`), and only a session that **both** primed and lasted `portForwardHealthySession`
resets the backoff. A never-primed session backs off toward `portForwardBlackHoleMaxDelay` (30 s):
retrying faster cannot fix a black hole, while every reconnect also tears down the xDS control stream
that shares the session. A later primed session clamps the delay straight back down.

After `portForwardBlackHoleThreshold` (3) consecutive never-primed sessions the condition is reported
(throttled to every 5 minutes) with its duration and a pointer to `kubevpn status`, which already
shows the connection as `unhealthy`.

### 3.5 Persist lease renewals independently of any stream (`pkg/xds`)

`LastRenew` was refreshed in three places and persisted in exactly one: `WatchTunIP`'s
`LeaseDuration/3` (100 s) ticker. A client whose port-forward dies every 30 s — and which waits
`ipWatcherRetryInterval` (10 s) before re-subscribing — never keeps a stream alive that long, so no
renewal was ever written down. The in-memory lease stayed fresh, the reaper correctly declined to
reclaim it, and the ConfigMap lied.

`renewLease` now marks the lease map dirty and the reaper's existing 30 s tick flushes it.
`GetTunIP`'s two inline writes go through `renewLease` as well, so there is a single place where the
field is touched. `saveAllocs` owns clearing the mark — before snapshotting, restored on failure — so
losing a renewal is impossible and the worst case is a redundant write.

`loadAllocs` released any allocation past `LeaseDuration` at startup, which for a frozen record means
releasing a **live** client's IP and letting the next client be handed the same address. It now
allows `loadGrace` (2 reap intervals) on top, covering both the flush interval and the moment a live
client needs to re-subscribe after a restart. The grace is deliberately small: a record hours stale
is indistinguishable from a dead client's, and that case is fixed at the source above.

## 4. What This Says About Testing

Both pre-existing tests covering route registration hand-crafted the announcement packet
(`TestReconnect_ProactiveRegistration` calls `sendFramedPacket` directly; the inter-client ICMP e2e
test has a `registerRoute()` helper). Neither ever executed `registrationPayloads()`. The mechanism
with no test was the one that broke.

`pkg/core/registration_integration_test.go` now drives the real data plane — 4 data slots plus the
control slot, over loopback TCP, into the real gvisor handler and `RouteHub` — and asserts automatic
registration, the heartbeat round trip, recovery after a port-forward teardown, and that a client
which cannot determine its own addresses both fails to register **and says so**.

## 5. Verification

Against a real cluster, after connecting:

```bash
# 4 pool conns per client, not 0
kubectl logs deploy/kubevpn-traffic-manager | grep -E 'Add route|Add pool conn'
# must be empty (was 731/hour)
kubectl logs deploy/kubevpn-traffic-manager | grep 'No route for stack output'
# lastRenew must advance at least every 30s
kubectl get cm kubevpn-traffic-manager -o jsonpath='{.data.TUN_ALLOCS}'
# a live client must keep its IP across a restart
kubectl rollout restart deploy/kubevpn-traffic-manager && kubevpn status
```

`Data plane never came up` should be absent from the client's `root_daemon.log`, and `kubevpn status`
should report `ok` rather than `unhealthy`.

## 6. Related Documents

- [03-dhcp-ip-allocation.md](03-dhcp-ip-allocation.md) — lease mechanism and `TUN_ALLOCS` persistence
- [08-heartbeat-health.md](08-heartbeat-health.md) — heartbeat, proactive registration, RouteHub
- [47-portforward-blackhole-liveness.md](47-portforward-blackhole-liveness.md) — the liveness watchdog
- [09-tun-ip-hot-update.md](09-tun-ip-hot-update.md) — why addresses are re-read rather than cached
