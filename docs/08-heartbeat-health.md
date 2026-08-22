# Heartbeat & Health Check Architecture

## Overview

KubeVPN uses 5 heartbeat/health check mechanisms across 4 layers to maintain connection liveness, detect failures, and trigger recovery. Each serves a distinct purpose — removing any one would leave a gap.

In addition, the client **observes the ICMP echo replies** to the #1 TUN heartbeat and exposes the last-reply time as the data-plane liveness signal driving `kubevpn status` (see [Data-Plane Liveness for `kubevpn status`](#data-plane-liveness-for-kubevpn-status)). This reuses an already-flowing heartbeat — it adds no extra traffic.

## Mechanism Inventory

```
┌─────────────────────────────────────────────────────────────────────┐
│ Layer 4: Application (business logic)                               │
│                                                                     │
│  #4 healthCheckGRPC          30s   gRPC health check (9002)        │
│  #5 Mapper.Run           informer  Pod/ConfigMap watch + 30s ticker │
│                                                                     │
├─────────────────────────────────────────────────────────────────────┤
│ Layer 3: TUN tunnel (application-layer keepalive)                   │
│                                                                     │
│  #1 TUN heartbeat            60s   ICMP to RouterIP, all slots     │
│  #3 TCP read/write deadline  180s/60s  Detect unresponsive conn    │
│                                                                     │
├─────────────────────────────────────────────────────────────────────┤
│ Layer 2: Transport (OS-level TCP keepalive)                         │
│                                                                     │
│  #2 TCP KeepAlive            60s   OS probes dead connections      │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

## Detailed Breakdown

### #1 TUN Heartbeat

**File:** `pkg/core/tun_client.go` — `ClientDevice.heartbeats()`

**What:** Sends ICMP Echo packets from the client's TUN IP to the router IP (`198.18.0.0`) every 60 seconds.

**Why necessary:**
- Registers the client's TUN IP in the server's RouteHub (via `hub.AddRoute(src, conn)`)
- Without this, the server doesn't know which connection belongs to which TUN IP — cross-client routing breaks
- Keeps all connection pool slots alive (heartbeats are broadcast to all slots)

**Broadcast to all slots:** Heartbeat packets have `dst == nil` and are cloned to all N connection pool slots. This was a bug fix — previously only slot 0 received heartbeats, causing slots 1-3 to timeout every 180s.

**Proactive registration on (re)connect:** Waiting up to a full 60s period for the next heartbeat to register a freshly reconnected conn would leave reverse traffic un-routable in the meantime. So each slot, immediately after dialing, writes a registration packet (an ICMP echo to the gateway carrying its TUN IP) **directly on the new conn** — `connSlot.run` via `clientTransport.registrationPayloads()`. The server registers the route from it at once, before any data. Being a direct write (not the `dst == nil` broadcast, which `trySendToSlot` may drop when a slot's channel is full), it is drop-immune. The echo reply also doubles as a liveness ping, so it refreshes the data-plane liveness signal on reconnect without any separate signalling path.

**Reply observation (data-plane liveness):** The server's gvisor stack replies to these echoes (`pkg/core/gvisor_icmp_forwarder.go`, preserving Ident/Sequence). The client's `readFromConn` recognizes echo replies from `RouterIP`/`RouterIP6` (`util.IsICMPEchoReplyFrom`), records the timestamp in `core.HeartbeatStats.MarkReply()`, and drops the packet (the OS would discard it anyway — the echo was crafted by us). A recent reply proves the tunnel carries traffic end-to-end to the server and back. See [Data-Plane Liveness for `kubevpn status`](#data-plane-liveness-for-kubevpn-status).

**Period:** 60s (`config.KeepAliveTime`)

### #2 TCP KeepAlive

**Files:** `pkg/core/transporter_tcp.go`, `pkg/core/connector_udp_over_tcp.go`

**What:** OS-level `SO_KEEPALIVE` on all tunnel TCP connections (both dialer and listener sides).

**Why necessary:** Detects half-open connections where one side has crashed without sending FIN/RST. The OS sends TCP keepalive probes and closes the connection if no response.

**Period:** 60s (`config.KeepAliveTime`)

**Relationship with #1:** #1 is application-layer (ICMP through TUN), #2 is transport-layer (TCP keepalive). #2 works even when #1's ICMP packets can't flow (e.g., gvisor stack is stuck). They are complementary, not redundant.

### #3 TCP Read/Write Deadline

**Files:** client — `pkg/core/tun_client.go` (`readFromConn()`, `writeToConn()`); server — `pkg/core/gvisor_tun_endpoint.go` (`readFromTCPConnWriteToEndpoint()`, `readFromEndpointWriteToTCPConn()`) and `pkg/core/conn_buffered_tcp.go` (`bufferedTCP.run()`).

**What:** Sets socket deadlines on tunnel TCP connections, on **both** ends. Read timeout is 180s, write timeout is 60s.

**Why necessary:** Detects application-level stalls where the TCP connection is alive but the remote side has stopped processing data.
- **Client side:** a stall triggers slot reconnection.
- **Server side:** a stall (e.g. a client that slept or had its NAT rebound, so no FIN/RST ever arrives) evicts the now-stale "ghost" conn from `RouteHub`. The read timeout exits `readFromTCPConnWriteToEndpoint`, whose deferred `RemoveRoutesByConn` deletes the route; the write timeout closes a conn whose half-open socket would otherwise buffer (and black-hole) reverse traffic until the kernel send buffer fills. Without these, a ghost conn lingered until OS keepalive (#2) eventually noticed — minutes of silently dropped reverse traffic. The two server writers (`bufferedTCP.run` and `readFromEndpointWriteToTCPConn`) share the same socket; both use the 60s timeout and refresh before each write, so neither observes the other's deadline as prematurely expired.

**Client-side liveness is active-in-either-direction (important).** The read (180s) deadline is the slot's idle/liveness detector, but a slot can be busy **sending** while receiving nothing: it hosts the per-slot inter-client gvisor stack (bound to its `subCtx` in `readFromConn`), whose output it writes out, while the reverse traffic (that flow's ACKs) is routed to a *different* pool slot — the server's `RouteMapTCP` is last-writer-wins and the #1 heartbeat, broadcast on every slot, makes the winning slot flap. So `writeToConn` also pushes the **read** deadline forward on each successful write (`UDPConnOverTCP` embeds the same conn, so both goroutines drive the one underlying read deadline; `SetReadDeadline` is safe with an in-flight `Read`). Without this, an actively-sending slot would hit the 180s read-idle timeout mid-transfer, be torn down, destroy its gvisor stack, and abort the transfer — observed as a mid-SCP disconnect (`read i/o timeout` → `operation aborted` → RST → RTO retransmit storm). A slot idle in **both** directions still times out.

**Optimization:** Deadlines are not reset on every packet. They are only refreshed when >50% of the timeout has elapsed, reducing syscalls from 1/packet to ~1/90s.

**Timeout values:**
- Read: `KeepAliveTime * 3 = 180s` — tolerates 2 missed heartbeat cycles
- Write: `KeepAliveTime = 60s` — writes should complete quickly

### #4 Control-Plane Health Check (gRPC over port-forward)

**File:** `pkg/handler/connect_tun.go` — `healthCheckGRPC()` (single-shot probe `probeControlPlaneGRPC()`)

**What:** Every 30s, dials the local control-plane port (port-forwarded from the traffic manager, `PortControlPlane` 9002) and issues a gRPC health `Check`, verifying the response is `SERVING`.

**Connection reuse:** `probeControlPlaneGRPC` reuses one `*grpc.ClientConn` across ticks, redialing only after a failure (the same single-shot probe also backs the data-plane status check — see below).

**Why necessary:** Validates that the port-forward to the traffic manager is alive and the control plane is serving.

**Failure action:** After 3 consecutive failures (with 10s backoff), calls `cancelFunc()` to tear down the port-forward, which triggers a reconnection in the `portForward()` retry loop.

**Period:** 30s, with 10s×3 retry backoff before declaring failure

> **Removed (dead code):** an earlier DNS-via-gudp variant `healthCheckPortForward()` (probed the data-plane path with a DNS query) was defined but never wired — only `healthCheckGRPC` is used — and has been deleted.

> **Removed:** an earlier `#5 ConfigMap Health Check` (`HealthPeriod`/`HealthStatus`, a 30s ConfigMap GET) was deleted. `kubevpn status` reflects data-plane liveness only (TUN + heartbeat, see below) and reads the proxy/sync list straight from the shared informer via `ConnectOptions.GetTrafficManagerConfigMap()`, so the health-status cache had no readers. See [11-configmap-informer.md](11-configmap-informer.md).

### #5 Mapper Reconciliation (Fargate mode)

**File:** `pkg/handler/proxy_mapper.go` — `Mapper.Run()`

**What:** Watches the traffic manager ConfigMap and pods via shared informers. When changes are detected, reconciles SSH reverse tunnels.

**Why necessary:** In Fargate mode, SSH tunnels must be established to new pods and torn down for deleted pods. Port mappings can change when envoy config is updated.

**Trigger:** Informer watch events (immediate) + 30s ticker fallback

## Timing Relationships

```
        0s      10s     20s     30s     40s     50s     60s     ...     180s
#4 gRPC |───────────────x───────────────x───────────────x          30s
#1 TUN  |───────────────────────────────────────────────x          60s
#2 TCP  |───────────────────────────────────────────────x          60s (OS)
#3 Read |───────────────────────────────────────────────────────x  180s deadline
```

## Failure Detection Matrix

| Failure | Detected by | Detection time |
|---------|-------------|----------------|
| Pod crash/restart | #4, #5 (watch event) | <1s (informer) or 30s (ticker) |
| Port-forward broken | #4 (DNS query via gudp) | 30s + retry |
| TUN tunnel stall | #3 (read deadline) | 180s |
| TCP connection half-open | #2 (OS keepalive) | ~60s |
| Slot idle (no traffic) | #1 (heartbeat broadcast) | 60s |
| Data plane not carrying traffic | heartbeat-reply staleness (`status`) | ~90s |

## API Server Load

| Mechanism | Requests/min (steady state) |
|-----------|---------------------------|
| #1 TUN heartbeat | 0 (no API calls) |
| #2 TCP KeepAlive | 0 (OS level) |
| #3 Read/Write deadline | 0 (socket level) |
| #4 gRPC health check | 0 (gRPC to control plane, not API) |
| #5 Mapper watch | 0 (informer, no polling) |
| **Total** | **~0 requests/min** |

All ConfigMap reads use the shared informer cache; `GetTrafficManagerConfigMap` does a one-off direct GET only when the cache is still cold.

## Data-Plane Liveness for `kubevpn status`

`kubevpn status` reports whether traffic actually flows through the TUN — not merely whether the TUN device exists or the API server is reachable. The signal is the freshness of the #1 heartbeat's echo reply. It deliberately does **not** factor in ConfigMap/API-server reachability (that is an orthogonal concern, not a data-plane signal).

### The sudo daemon owns the verdict

The decision inputs — TUN device presence and heartbeat-reply freshness — are both available in the **root (sudo) daemon**, which owns the TUN and observes the replies. So the sudo daemon computes the full `connected` / `unhealthy` / `disconnected` verdict itself, in `deriveConnectionStatus`:

```go
func deriveConnectionStatus(tunUp bool, lastHeartbeat time.Time) string {
    if !tunUp { return StatusFailed }                                  // disconnected
    if lastHeartbeat.IsZero() ||
        time.Since(lastHeartbeat) > heartbeatStaleThreshold { return StatusUnhealthy }
    return StatusOk                                                    // connected
}
```

`heartbeatStaleThreshold = config.KeepAliveTime * 3 / 2` (90s) — tolerates one missed beat's jitter. Worst-case detection latency is ~90s; recovery is fast because each slot proactively re-announces its route directly on the new conn the moment it reconnects (see #1 *Proactive registration on (re)connect*), and that announcement's echo reply also refreshes the liveness signal.

### The user daemon reuses the verdict (no new proto field)

`status` is served by the user daemon, which has neither the TUN heartbeat nor the data plane. Rather than ship a raw timestamp and recompute, the user daemon **reuses the `Status` string the sudo daemon already computed** — carried on the existing `Status` field (no new proto field). This mirrors `resolveTunIP`'s user/sudo split:

```
root daemon (data plane)                       user daemon (control plane)
─────────────────────────                      ───────────────────────────
clientTransport.readFromConn
  └─ HeartbeatStats.MarkReply()                Status RPC
       ▲                                          └─ getSudoTunIPs() ── queries sudo Status
NetworkManager.LastHeartbeat()                        │  reads Status (the verdict string)
       ▲                                              ▼
ConnectOptions.GetLastHeartbeat()              resolveStatus(connect, ips, tunUp)
       ▲                                          └─ user: ip.status from sudo
deriveConnectionStatus(tunUp, lastHeartbeat)      └─ sudo: deriveConnectionStatus(...)
  → sets Status string (sudo daemon)
```

- Sudo daemon: `buildConnectionStatus` → `resolveStatus` (no map) → `deriveConnectionStatus(tunUp, connect.GetLastHeartbeat())`.
- User daemon: `getSudoTunIPs` reads the sudo `Status` string into the per-connection `tunIP` map; `resolveStatus` returns it (or `disconnected` if the sudo daemon doesn't know the connection).

### Why heartbeat reply (vs an active probe)

Reusing the in-flight heartbeat adds **zero** traffic. The trade-off is that it validates reachability to the server **gateway** (`RouterIP`), not a specific Service ClusterIP or the control plane's serving state.
