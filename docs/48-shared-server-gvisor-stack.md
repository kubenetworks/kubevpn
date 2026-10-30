# Per-Client Gvisor Stack with Control/Data Plane Separation

## 1. Problem

The server (traffic manager) needs to balance three concerns:

1. **Session continuity**: pool-slot tunnel drops must not kill gvisor TCP sessions (SSH, SCP).
2. **Client isolation**: one slow client must not block other clients' gvisor stacks.
3. **Heartbeat request isolation**: business-queue congestion should not prevent sending probes.
   Replies and the outer transport still depend on data-path progress, so this cannot guarantee
   liveness during congestion.

Additionally, packet drops on the client side (from `trySendToSlot` when a slot buffer is full)
cause OS TCP to enter RTO exponential backoff (1–7 s stalls), degrading SCP throughput.

## 2. Design

### 2.1 Per-Client Gvisor Stack (Server Side)

Each client (identified by TUN IP) gets an independent gvisor stack whose lifetime is the
**client session** — outliving individual pool-slot connections but isolated from other clients.

```
Client A (1 data slot) ─┐
                         ├→ Stack-A (lifetime: client A session) → TCPForwarder → cluster
Client A reconnects ─────┘

Client B (1 data slot) ─┐
                         ├→ Stack-B (lifetime: client B session) → TCPForwarder → cluster
Client B reconnects ─────┘

Inter-client (A→B): direct forwarding via RouteHub (no gvisor stack involved)
```

### 2.2 Control/Data Plane Separation (Client Side)

```
Data Plane:
  TUN → routeOutbound → tunInbound → runConnPool → slot[0].inbound (blocking) → writeToConn
                                                         ↑ backpressure propagates to OS TCP

Control Plane:
  heartbeats() → controlSlot.inbound → writeToConn (independent TCP conn)
                      ↑ requests bypass the business queue
```

| Connection | Type Prefix | Server Behavior | RouteHub |
|------------|-------------|-----------------|----------|
| Data slot (×1) | `packetTypeToGvisor` (0x01) | `AddRoute` + gvisor inject | Registered |
| Control slot (×1) | `packetTypeControl` (0x02) | `handleControlConn` | NOT registered |

Both conns use the **same TCP forwarding endpoint 10801**, not xDS 9002. SSH/port-forward
still shares an outer TCP transport. The default uses direct slot-0 dispatch with no
slot-selection parsing/hash; the private data-only `poolSize` override supports nil-stats
1/2/4/8-slot comparisons, with no new CLI or environment flag. `connSlot` lifecycle, queues,
deadlines and batching are retained, as is concurrent handling of business flows.

## 3. Protocol: `packetTypeControl`

```go
packetTypeToTUN    byte = 0  // gvisor-processed → write to TUN
packetTypeToGvisor byte = 1  // raw IP → inject into gvisor stack
packetTypeControl  byte = 2  // control frame (heartbeat ICMP)
```

The **first datagram** on a conn declares its type:
- Data conn: first packet has prefix 0x00 or 0x01 → normal data path
- Control conn: first packet has prefix 0x02 → server enters `handleControlConn`

### Server `handleControlConn`

- Does **NOT** call `AddRoute` (conn never appears in RouteHub)
- Does **NOT** participate in data routing
- Reads heartbeat ICMP echo → injects into per-client gvisor stack → stack generates reply
- Reply routes back via registered data conns (through RouteHub): first writable with fallback,
  not per-flow load balancing; the default has one registered conn per client address in steady state
- Read deadline: KeepAliveTime×3 (5 s heartbeats normally keep it active; stalls can still time out)

### Client `controlSlot`

- `isControl: true` → sends `packetTypeControl` declaration on connect
- No `registrations` (no route announcement needed)
- No `interClientInbound` (pure control, no data)
- `heartbeats()` writes ONLY to controlSlot

## 4. Data Plane: Blocking Backpressure

`runConnPool` uses blocking channel sends for data packets:

```go
select {
case t.slots[0].inbound <- packet: // default; only multi-slot tests hash for selection
case <-ctx.Done():
    packet.release()
    return
}
```

Backpressure chain: slot full → runConnPool blocks → tunInbound full → routeOutbound blocks →
pumpTun stops reading TUN → OS TCP send buffer fills → TCP advertises smaller window → SCP
application slows down (smooth degradation, no RTO).

Previously `trySendToSlot` silently dropped packets → OS TCP saw loss → RTO exponential backoff
(1–7 s stalls). The blocking design eliminates this entirely.

## 5. Per-Client Stack Lifecycle

| Event | Action |
|-------|--------|
| First cluster-bound packet from new client IP | Create stack + reader goroutine |
| Pool slot reconnects | conn re-registered in RouteHub; stack untouched |
| All data conns for client removed | Start grace timer (KeepAliveTime×3 = 180 s) |
| Grace timer fires (no reconnect) | Destroy stack, free memory |
| Server shutdown | All stacks destroyed |

### Why 180 s grace period

Port-forward reconnections typically complete in 10–15 s. The grace period (3× `KeepAliveTime`, not the 5 s heartbeat interval)
covers network glitches and brief laptop sleep. After 180 s without any data conn, the client
is genuinely gone and its stack memory can be reclaimed.

### Data slot timeout and session retention

1. The watchdog requires a correlated reply over a registered data conn; control requests alone
   do not prove liveness.
2. With the single-data-slot default, losing that conn temporarily removes the return route;
   there are no other active data slots to guarantee uninterrupted routing.
3. The slot auto-reconnects (`connSlot.run` loop) and registers its route immediately.
4. The shared per-client gvisor stack and grace timer retain TCP session state across brief gaps,
   including idle sessions. Retention does not depend on four slots and cannot guarantee that
   application timeouts or a prolonged outage will not end a session.

## 6. Safety Properties

### 6.1 Session continuity

A pool-slot drop removes one conn from RouteHub but the client's stack is intact. Gvisor's TCP
state (seq, window, congestion) survives. When the slot reconnects, responses flow through the
new conn.

### 6.2 Client isolation

Each client's `readFromEndpointWriteToRoute` is independent. Blocking `WriteToRoutePacket` on
one client stalls only that client's gvisor stack. Other clients are unaffected.

### 6.3 Heartbeat independence

`controlSlot` has its own queue and TCP conn, so heartbeat requests bypass the business queue.
It uses the same forwarding endpoint (10801), not xDS (9002), and the outer SSH/port-forward
TCP transport is still shared. Replies return through the registered data conn and must pass
`HeartbeatStats.observeReply` correlation checks. Data congestion or an outer-transport stall
can therefore still delay replies and trigger recovery; there is no absolute liveness guarantee.

## 7. Implementation

### Key types

```go
type clientStack struct {
    endpoint   *channel.Endpoint
    stack      *stack.Stack
    cancel     context.CancelFunc
    graceTimer *time.Timer
}

type gvisorTCPHandler struct {
    hub      *RouteHub
    newStack stackConstructor
    ctx      context.Context
    mu       sync.Mutex
    clients  map[string]*clientStack
}

type connSlot struct {
    ...
    isControl bool  // true = control conn (heartbeat only, no RouteHub)
}
```

### Files

| File | Role |
|------|------|
| `packet.go` | `packetTypeControl` constant |
| `gvisor_tcp_handler.go` | Per-client stack map, `handleControlConn`, grace timer |
| `gvisor_tun_endpoint.go` | First-packet type detection, per-client endpoint injection |
| `tun_client.go` | controlSlot, blocking runConnPool, heartbeats via controlSlot |
| `conn_slot.go` | `isControl` field, control declaration on connect |
| `route.go` | `OnRouteEmpty`/`OnRouteAdded` callbacks |

## 8. Resource Usage

- Per-client stack ≈ 2–4 MB (TCP buffers, forwarder goroutines)
- 50 clients ≈ 100–200 MB; traffic manager pod (512 MB–1 GB) handles this comfortably
- 2 conns per client by default (1 data + 1 control), down from the historical 4 data + 1 control
- Each client: 1 stack reader goroutine (lightweight)

## 9. Related

- [03-dhcp-ip-allocation.md](03-dhcp-ip-allocation.md) — per-client TUN IP uniqueness
- [08-heartbeat-health.md](08-heartbeat-health.md) — heartbeat protocol, KeepAliveTime
- [41-five-tuple-pool-dispatch.md](41-five-tuple-pool-dispatch.md) — slot hash design
- [47-portforward-blackhole-liveness.md](47-portforward-blackhole-liveness.md) — client-side liveness watchdog
