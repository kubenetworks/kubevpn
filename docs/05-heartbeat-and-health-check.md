# Heartbeat & Health Check Architecture

## Overview

KubeVPN uses 8 heartbeat/health check mechanisms across 5 layers to maintain connection liveness, detect failures, and trigger recovery. Each serves a distinct purpose — removing any one would leave a gap.

## Mechanism Inventory

```
┌─────────────────────────────────────────────────────────────────────┐
│ Layer 5: Application (business logic)                               │
│                                                                     │
│  #4 healthCheckPortForward   30s   DNS query through gvisor tunnel  │
│  #5 healthCheckTCPConn       30s   DNS query direct to pod          │
│  #6 HealthPeriod             30s   ConfigMap GET (API reachability) │
│  #8 Mapper.Run           informer  Pod/ConfigMap watch + 30s ticker │
│                                                                     │
├─────────────────────────────────────────────────────────────────────┤
│ Layer 4: TUN tunnel (application-layer keepalive)                   │
│                                                                     │
│  #1 TUN heartbeat            60s   ICMP to RouterIP, all slots     │
│  #3 TCP read/write deadline  180s/60s  Detect unresponsive conn    │
│                                                                     │
├─────────────────────────────────────────────────────────────────────┤
│ Layer 3: Transport (OS-level TCP keepalive)                         │
│                                                                     │
│  #2 TCP KeepAlive            60s   OS probes dead connections      │
│                                                                     │
├─────────────────────────────────────────────────────────────────────┤
│ Layer 2: SSH tunnel                                                 │
│                                                                     │
│  #7 SSH KeepAlive            10s   SSH keep-alive request          │
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

```go
// runConnPool distributes heartbeats to ALL slots
if packet.dst != nil {
    d.slots[ipHash(packet.dst, n)] <- packet
} else {
    d.slots[0] <- packet
    for i := 1; i < n; i++ {
        clone := config.LPool.Get().([]byte)
        copy(clone, packet.data[:packet.length+2])
        d.slots[i] <- &Packet{data: clone, length: packet.length}
    }
}
```

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

```go
if time.Until(nextDeadline) < readTimeout/2 {
    nextDeadline = time.Now().Add(readTimeout)
    conn.SetReadDeadline(nextDeadline)
}
```

**Timeout values:**
- Read: `KeepAliveTime * 3 = 180s` — tolerates 2 missed heartbeat cycles
- Write: `KeepAliveTime = 60s` — writes should complete quickly

### #4 Port-Forward DNS Health Check (gvisor path)

**File:** `pkg/handler/connect_tun.go` — `healthCheckPortForward()`

**What:** Every 30s, dials the local gvisor UDP port, sends a DNS query for `kubevpn-traffic-manager` through the TUN tunnel, and verifies a response comes back.

**Why necessary:** Validates the entire data path is working end-to-end:
```
local gvisor port → TUN tunnel → port-forward → traffic manager pod → DNS server → response
```
If this fails, port-forward is re-established.

**Failure action:** Calls `cancelFunc()` to tear down the port-forward, which triggers a reconnection in the `portForward()` retry loop.

**Period:** 30s, with 10s×3 retry backoff before declaring failure

### #5 Port-Forward TCP Health Check (direct path)

**File:** `pkg/handler/connect_tun.go` — `healthCheckTCPConn()`

**What:** Every 30s, directly TCP-connects to the pod's IP:53 and sends a DNS query. Bypasses the TUN tunnel entirely.

**Why necessary:** Catches failures that #4 would miss — if the TUN tunnel itself is corrupted but port-forward is fine, #4 fails but #5 succeeds. Conversely, if the pod is unreachable, both fail and trigger reconnection.

**Difference from #4:** #4 tests the gvisor tunnel path, #5 tests direct pod reachability. Together they pinpoint whether the issue is in the tunnel or the pod.

**Period:** 30s, with 10s×3 retry backoff

### #6 ConfigMap Health Check (API reachability)

**File:** `pkg/handler/healthchecker.go` — `HealthPeriod()`

**What:** Every 30s, syncs the traffic manager ConfigMap from the shared informer cache and does a direct GET to verify API server reachability.

**Why necessary:**
- Caches the ConfigMap in `healthStatus.cm` for `kubevpn status` queries
- The direct GET detects API server unreachable (informer watch reconnects silently)
- Status reporting (`daemon/action/status.go`) reads proxy rules from the cached ConfigMap

**Period:** 30s (informer cache reads are instant, direct GET is the fallback)

### #7 SSH KeepAlive

**File:** `pkg/ssh/config.go` — `keepAlive()`

**What:** Sends `keepalive@golang.org` SSH request every 10s on SSH tunnel connections.

**Why necessary:** SSH connections through NAT/firewalls are dropped after idle timeout (often 30-60s). Without keepalive, the SSH tunnel dies silently.

**Period:** 10s — shorter than typical NAT timeout (30s)

### #8 Mapper Reconciliation (Fargate mode)

**File:** `pkg/handler/proxy.go` — `Mapper.Run()`

**What:** Watches the traffic manager ConfigMap and pods via shared informers. When changes are detected, reconciles SSH reverse tunnels.

**Why necessary:** In Fargate mode, SSH tunnels must be established to new pods and torn down for deleted pods. Port mappings can change when envoy config is updated.

**Trigger:** Informer watch events (immediate) + 30s ticker fallback

## Timing Relationships

```
        0s      10s     20s     30s     40s     50s     60s     ...     180s
#7 SSH  |───x───x───x───x───x───x───x───x───x───x───x───x───x    10s
#4 PF   |───────────────x───────────────x───────────────x          30s
#5 TCP  |───────────────x───────────────x───────────────x          30s
#6 CM   |───────────────x───────────────x───────────────x          30s
#1 TUN  |───────────────────────────────────────────────x          60s
#2 TCP  |───────────────────────────────────────────────x          60s (OS)
#3 Read |───────────────────────────────────────────────────────x  180s deadline
```

## Failure Detection Matrix

| Failure | Detected by | Detection time |
|---------|-------------|----------------|
| Pod crash/restart | #4, #5, #8 (watch event) | <1s (informer) or 30s (ticker) |
| Port-forward broken | #4, #5 | 30s + retry |
| TUN tunnel stall | #3 (read deadline) | 180s |
| TCP connection half-open | #2 (OS keepalive) | ~60s |
| SSH tunnel NAT timeout | #7 | 10s |
| API server unreachable | #6 | 30s |
| Slot idle (no traffic) | #1 (heartbeat broadcast) | 60s |

## API Server Load

| Mechanism | Requests/min (steady state) |
|-----------|---------------------------|
| #1 TUN heartbeat | 0 (no API calls) |
| #2 TCP KeepAlive | 0 (OS level) |
| #3 Read/Write deadline | 0 (socket level) |
| #4 Port-forward DNS | 0 (uses tunnel, no API) |
| #5 TCP DNS | 0 (direct to pod) |
| #6 ConfigMap GET | 2/min (30s fallback ticker) |
| #7 SSH keepalive | 0 (SSH protocol) |
| #8 Mapper watch | 0 (informer, no polling) |
| **Total** | **~2 requests/min** |

All ConfigMap reads use the shared informer cache. Only the #6 fallback ticker does a direct API GET (for connectivity verification).
