# Per-Client Gvisor Stack with Control/Data Plane Separation

## 1. Problem

The server (traffic manager) needs to balance three concerns:

1. **Session continuity**: pool-slot tunnel drops must not kill gvisor TCP sessions (SSH, SCP).
2. **Client isolation**: one slow client must not block other clients' gvisor stacks.
3. **Liveness independence**: data-plane congestion must not starve heartbeat probes, which would
   cause the watchdog to misjudge the tunnel as dead and trigger a reconnect mid-transfer.

Additionally, packet drops on the client side (from `trySendToSlot` when a slot buffer is full)
cause OS TCP to enter RTO exponential backoff (1–7 s stalls), degrading SCP throughput.

## 2. Design

### 2.1 Per-Client Gvisor Stack (Server Side)

Each client (identified by TUN IP) gets an independent gvisor stack whose lifetime is the
**client session** — outliving individual pool-slot connections but isolated from other clients.

```
Client A (4 data slots) ─┐
                         ├→ Stack-A (lifetime: client A session) → TCPForwarder → cluster
Client A reconnects ─────┘

Client B (4 data slots) ─┐
                         ├→ Stack-B (lifetime: client B session) → TCPForwarder → cluster
Client B reconnects ─────┘

Inter-client (A→B): direct forwarding via RouteHub (no gvisor stack involved)
```

### 2.2 Control/Data Plane Separation (Client Side)

```
Data Plane:
  TUN → routeOutbound → tunInbound → runConnPool → slots[0..3].inbound (blocking) → writeToConn
                                                         ↑ backpressure propagates to OS TCP

Control Plane:
  heartbeats() → controlSlot.inbound → writeToConn (independent TCP conn)
                      ↑ never blocked by data congestion
```

| Connection | Type Prefix | Server Behavior | RouteHub |
|------------|-------------|-----------------|----------|
| Data slots (×4) | `packetTypeToGvisor` (0x01) | `AddRoute` + gvisor inject | Registered |
| Control slot (×1) | `packetTypeControl` (0x02) | `handleControlConn` | NOT registered |

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
- Reply routes back via data conns (through RouteHub)
- Read deadline: KeepAliveTime×3 (client sends heartbeat every 5 s, never times out)

### Client `controlSlot`

- `isControl: true` → sends `packetTypeControl` declaration on connect
- No `registrations` (no route announcement needed)
- No `interClientInbound` (pure control, no data)
- `heartbeats()` writes ONLY to controlSlot

## 4. Data Plane: Blocking Backpressure

`runConnPool` uses blocking channel sends for data packets:

```go
select {
case t.slots[flowHash(key, packet.dst, n)].inbound <- packet:
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

Port-forward reconnections typically complete in 10–15 s. The grace period (3× heartbeat cycle)
covers network glitches and brief laptop sleep. After 180 s without any data conn, the client
is genuinely gone and its stack memory can be reclaimed.

### Why idle data slot timeout is safe

1. Watchdog only checks `HeartbeatStats.LastReply()`, driven by controlSlot → never misjudges
2. Other active data conns remain in RouteHub → routing unaffected
3. Slot auto-reconnects (`connSlot.run` loop), registers route immediately on reconnect
4. No in-flight TCP sessions on an idle slot (sessions hash to the active slots)

## 6. Safety Properties

### 6.1 Session continuity

A pool-slot drop removes one conn from RouteHub but the client's stack is intact. Gvisor's TCP
state (seq, window, congestion) survives. When the slot reconnects, responses flow through the
new conn.

### 6.2 Client isolation

Each client's `readFromEndpointWriteToRoute` is independent. Blocking `WriteToRoutePacket` on
one client stalls only that client's gvisor stack. Other clients are unaffected.

### 6.3 Heartbeat independence

controlSlot has its own TCP conn. Even if all 4 data slots are congestion-blocked for 30 s,
heartbeat ICMP still flows → `MarkReply()` still fires → watchdog sees liveness → no false
reconnect trigger.

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
- 5 conns per client (4 data + 1 control) vs previous 4
- Each client: 1 stack reader goroutine (lightweight)

## 9. Related

- [03-dhcp-ip-allocation.md](03-dhcp-ip-allocation.md) — per-client TUN IP uniqueness
- [08-heartbeat-health.md](08-heartbeat-health.md) — heartbeat protocol, KeepAliveTime
- [41-five-tuple-pool-dispatch.md](41-five-tuple-pool-dispatch.md) — slot hash design
- [47-portforward-blackhole-liveness.md](47-portforward-blackhole-liveness.md) — client-side liveness watchdog
