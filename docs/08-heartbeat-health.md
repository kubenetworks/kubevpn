# Heartbeat & Health Check Architecture

## Overview

KubeVPN uses 6 heartbeat/health check mechanisms across 4 layers to maintain connection liveness, detect failures, and trigger recovery. Each serves a distinct purpose — removing any one would leave a gap.

## Mechanism Inventory

```
┌─────────────────────────────────────────────────────────────────────┐
│ Layer 4: Application (business logic)                               │
│                                                                     │
│  #4 healthCheckGRPC          30s   gRPC health check (9002)        │
│  #5 HealthPeriod             30s   ConfigMap GET (API reachability) │
│  #6 Mapper.Run           informer  Pod/ConfigMap watch + 30s ticker │
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

**Period:** 60s (`config.KeepAliveTime`)

### #2 TCP KeepAlive

**Files:** `pkg/core/transporter_tcp.go`, `pkg/core/connector_udp_over_tcp.go`

**What:** OS-level `SO_KEEPALIVE` on all tunnel TCP connections (both dialer and listener sides).

**Why necessary:** Detects half-open connections where one side has crashed without sending FIN/RST. The OS sends TCP keepalive probes and closes the connection if no response.

**Period:** 60s (`config.KeepAliveTime`)

**Relationship with #1:** #1 is application-layer (ICMP through TUN), #2 is transport-layer (TCP keepalive). #2 works even when #1's ICMP packets can't flow (e.g., gvisor stack is stuck). They are complementary, not redundant.

### #3 TCP Read/Write Deadline

**File:** `pkg/core/tun_client.go` — `readFromConn()`, `writeToConn()`

**What:** Sets socket deadlines on tunnel TCP connections. Read timeout is 180s, write timeout is 60s.

**Why necessary:** Detects application-level stalls where the TCP connection is alive but the remote side has stopped processing data. Triggers slot reconnection.

**Optimization:** Deadlines are not reset on every packet. They are only refreshed when >50% of the timeout has elapsed, reducing syscalls from 1/packet to ~1/90s.

**Timeout values:**
- Read: `KeepAliveTime * 3 = 180s` — tolerates 2 missed heartbeat cycles
- Write: `KeepAliveTime = 60s` — writes should complete quickly

### #4 Data Plane Health Check (DNS query via gudp relay)

**File:** `pkg/handler/connect_tun.go` — `healthCheckPortForward()`

**What:** Every 30s, sends a DNS query through the gudp relay (port-forwarded from traffic manager) to verify the full data-plane path is alive (port-forward + gudp + DNS container).

**Connection reuse:** TCP conn and PacketConn are held outside the checker closure. On failure, `closeConn()` cleans up and sets to nil; the next check rebuilds. `defer closeConn()` at function end ensures resource release.

**Why necessary:** Validates the data plane is reachable via port-forward. Uses DNS queries instead of gRPC `GetTunIP` to avoid triggering silent IP reallocation as a side effect of health checking.

**Failure action:** After 3 consecutive failures (with 10s backoff), calls `cancelFunc()` to tear down the port-forward, which triggers a reconnection in the `portForward()` retry loop.

**Period:** 30s, with 10s×3 retry backoff before declaring failure

### #5 ConfigMap Health Check (API reachability)

**File:** `pkg/handler/configmap_store.go` — `HealthPeriod()`

**What:** Every 30s, syncs the traffic manager ConfigMap from the shared informer cache and does a direct GET to verify API server reachability.

**Thread safety:** `ConfigMapStore` uses `healthMu sync.RWMutex` to protect `healthStatus` — `syncFromCache` and `HealthCheckOnce` write with `Lock`, `GetHealthStatus` reads with `RLock`.

**Why necessary:**
- Caches the ConfigMap in `healthStatus.cm` for `kubevpn status` queries
- The direct GET detects API server unreachable (informer watch reconnects silently)
- Status reporting (`daemon/action/status.go`) reads proxy rules from the cached ConfigMap

**Period:** 30s (informer cache reads are instant, direct GET is the fallback)

### #6 Mapper Reconciliation (Fargate mode)

**File:** `pkg/handler/proxy_mapper.go` — `Mapper.Run()`

**What:** Watches the traffic manager ConfigMap and pods via shared informers. When changes are detected, reconciles SSH reverse tunnels.

**Why necessary:** In Fargate mode, SSH tunnels must be established to new pods and torn down for deleted pods. Port mappings can change when envoy config is updated.

**Trigger:** Informer watch events (immediate) + 30s ticker fallback

## Timing Relationships

```
        0s      10s     20s     30s     40s     50s     60s     ...     180s
#4 gRPC |───────────────x───────────────x───────────────x          30s
#5 CM   |───────────────x───────────────x───────────────x          30s
#1 TUN  |───────────────────────────────────────────────x          60s
#2 TCP  |───────────────────────────────────────────────x          60s (OS)
#3 Read |───────────────────────────────────────────────────────x  180s deadline
```

## Failure Detection Matrix

| Failure | Detected by | Detection time |
|---------|-------------|----------------|
| Pod crash/restart | #4, #5, #6 (watch event) | <1s (informer) or 30s (ticker) |
| Port-forward broken | #4 (DNS query via gudp) | 30s + retry |
| TUN tunnel stall | #3 (read deadline) | 180s |
| TCP connection half-open | #2 (OS keepalive) | ~60s |
| API server unreachable | #5 | 30s |
| Slot idle (no traffic) | #1 (heartbeat broadcast) | 60s |

## API Server Load

| Mechanism | Requests/min (steady state) |
|-----------|---------------------------|
| #1 TUN heartbeat | 0 (no API calls) |
| #2 TCP KeepAlive | 0 (OS level) |
| #3 Read/Write deadline | 0 (socket level) |
| #4 gRPC health check | 0 (gRPC to control plane, not API) |
| #5 ConfigMap GET | 2/min (30s fallback ticker) |
| #6 Mapper watch | 0 (informer, no polling) |
| **Total** | **~2 requests/min** |

All ConfigMap reads use the shared informer cache. Only the #5 fallback ticker does a direct API GET (for connectivity verification).
