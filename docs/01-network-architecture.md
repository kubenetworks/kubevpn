# KubeVPN Network Architecture

## Overview

KubeVPN creates a bidirectional network tunnel between a local machine and a Kubernetes cluster. All IP traffic (TCP, UDP, ICMP) flows through a TUN device on the client, gets encapsulated over TCP, and is forwarded via `kubectl port-forward` to a traffic manager pod running a userspace network stack (gvisor).

> **Route source:** the cluster pod/service **CIDRs** are routed to the TUN device at connect
> time. Per-pod route prefixes are discovered **server-side** by the traffic manager and
> pushed to the client over gRPC (the client no longer runs its own cluster-wide pod/service
> list-watch) — see [44-server-side-route-discovery.md](44-server-side-route-discovery.md).

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│ Local Machine                                                                    │
│                                                                                  │
│  ┌─────────┐     ┌──────────────┐     ┌────────────┐     ┌──────────────────┐  │
│  │  App    │────▶│  TUN Device  │────▶│  Client    │────▶│  port-forward    │  │
│  │(curl..) │     │ (utun/tun0)  │     │  Engine    │     │  (TCP tunnel)    │  │
│  └─────────┘     └──────────────┘     └────────────┘     └────────┬─────────┘  │
│                                                                     │            │
└─────────────────────────────────────────────────────────────────────┼────────────┘
                                                                      │
                                                          kubectl port-forward
                                                                      │
┌─────────────────────────────────────────────────────────────────────┼────────────┐
│ Kubernetes Cluster                                                   │            │
│                                                                      ▼            │
│  ┌───────────────────────────────────────────────────────────────────────────┐   │
│  │  Traffic Manager Pod (kubevpn-traffic-manager)                             │   │
│  │                                                                            │   │
│  │  ┌─────────────┐     ┌──────────────┐     ┌────────────────────────────┐ │   │
│  │  │  TCP Server │────▶│ Gvisor Stack │────▶│  TCP/UDP Forwarder         │ │   │
│  │  │  (port 10801)     │ (userspace)  │     │  (dial real service)       │ │   │
│  │  └─────────────┘     └──────────────┘     └────────────────────────────┘ │   │
│  │         │                                                                  │   │
│  │         ▼                                                                  │   │
│  │  ┌─────────────┐                                                          │   │
│  │  │  TUN Device │◀───── Pod/Service traffic                                │   │
│  │  │  (server)   │                                                           │   │
│  │  └─────────────┘                                                           │   │
│  └───────────────────────────────────────────────────────────────────────────┘   │
└──────────────────────────────────────────────────────────────────────────────────┘
```

## Wire Protocol

### Transport Layer

IP packets are encapsulated in a custom datagram framing protocol over TLS/TCP. TLS is mandatory — if TLS config parsing fails with an error other than `ErrNoTLSConfig` (which indicates an intentional no-TLS setup), both client and server refuse to start rather than silently downgrading to plaintext.

```
┌──────────────────────────────────────────┐
│         Datagram Frame (on TCP)          │
├──────────┬────────┬──────────────────────┤
│ Length   │ Prefix │      IP Packet       │
│ (2 bytes)│(1 byte)│   (variable length)  │
├──────────┼────────┼──────────────────────┤
│ uint16   │ 0/1/2  │  IPv4/IPv6 packet    │
│ big-end  │        │                      │
└──────────┴────────┴──────────────────────┘
```

- **Length** (2 bytes): big-endian uint16, value = len(prefix) + len(IP packet)
- **Prefix** (1 byte): routing directive (see below)
- **IP Packet**: raw IPv4 or IPv6 packet

The framing layer accepts uint16 payload lengths **0..65535**; tunnel payloads include the
prefix within that length. `readDatagramPacket` reads the two-byte header into independent
storage, then reads the body into the caller's buffer. An undersized buffer returns
`io.ErrShortBuffer`; the consumed header makes the stream unusable, so the framing wrapper
closes it rather than trying to resume. Outbound payloads above 65535 are rejected before
any write. Specifically, `UDPConnOverTCP.Write` uses standalone storage for its largest
legal frame (65537 bytes), without returning that storage to `config.LPool`. This is the
wrapper's allocation contract, not a guarantee for every framed-packet path. The uint16
wire limit is unchanged; other pooled paths can reject packets when their buffer is too short.

`writeFull` continues positive short writes on the **same connection**, reports actual wire
progress, and preserves errors (zero progress without an error becomes `io.ErrShortWrite`).
A partially written frame is never replayed on another connection. Read and Write are
serialized separately by `UDPConnOverTCP`; context cancellation/Close closes the transport
without waiting for an I/O mutex. These are safety contracts, not a wire-format change.

### Prefix Byte Semantics

| Value | Name | Direction | Meaning |
|-------|------|-----------|---------|
| `1` | INJECT | client→server | Inject into gvisor stack for TCP/UDP processing |
| `1` | INJECT | server→client | Inject into client gvisor stack (rare: self-to-self traffic) |
| `0` | DIRECT | server→client | Already processed by gvisor; write directly to TUN device |
| `0` | FORWARD | server internal | Forward to another client via RouteHub (multi-client routing) |
| `2` | CONTROL | client→server | Heartbeat control conn; handled separately, never registered in RouteHub |

**Routing decision on server (data conn, per packet; control conns use `handleControlConn`):**
```go
// Simplified: registered peer destinations route directly before prefix handling.
if hub.HasRoute(dstKey) {
    _, err = hub.WriteToRoutePacket(ctx, dstKey, packet)
    packet.release() // caller's reference
} else if prefix == packetTypeToGvisor {
    // Inject into the per-client stack → TCP/UDP forwarder → real service.
    endpoint.InjectInbound(protocol, pkt)
} else if !hub.enqueueTUN(ctx, packet) {
    // Private TUN bridge did not take ownership (canceled/stopped).
    packet.release()
}
```

**Routing decision on client (per packet from server):**
```go
if buf[0] == 1 {
    // Rare: needs client-side gvisor processing
    gvisorInbound <- packet
} else {
    // Normal: write to TUN device for local app consumption
    tunOutbound <- packet
}
```

## Connection Pool Architecture

### Design

The client defaults to **one data `connSlot` plus one independent control slot**:

```
TUN read → runConnPool → slot[0] → data conn ───────┐
                                                 ├→ SSH / port-forward → TM:10801
heartbeats() → controlSlot → control conn ────────┘

Response path (including heartbeat replies):
per-client gvisor stack → RouteHub → registered data conn
  → readFromConn → tunOutbound → writeToTun → TUN device
```

Protocol-owned heartbeat replies are consumed by the client rather than written to TUN.
Default outbound dispatch goes directly to slot 0, without parsing or hashing for slot
selection. `connSlot` retains its queue, dial/reconnect lifecycle, read/write loops, deadlines
and bounded batching. One tunnel data connection still multiplexes concurrent business flows;
it does not serialize application requests or reduce business concurrency to one.

The control slot has a separate queue and connection to the **same TCP forwarding endpoint
10801**, not xDS port 9002. Its requests bypass the business queue, but replies use the
registered data conn. SSH/port-forward still shares an outer TCP transport, so separation
is not an absolute liveness guarantee.

### Hash Function (Multi-Slot Test Override)

The private, data-only `poolSize` override is retained for nil-stats tests and benchmarks
comparing 1/2/4/8 data slots. Only the multi-slot path uses the following hash; production
single-slot dispatch skips it. There is no CLI or environment flag for pool size.

A stateless FNV-1a hash of the flow's **five-tuple** — `(proto, dst IP, src port, dst port)`
— mod N. Distinct connections to the same destination IP differ in source port, so they
spread across slots instead of collapsing onto one. Packets with no usable L4 ports (ICMP,
IP fragments, unparsed IPv6 extension headers) fall back to hashing the destination IP only
(`ipHash`), preserving legacy behavior and keeping all fragments of one datagram together.

The source IP is omitted (a client has a single TUN IP, so it adds nothing — the IPVS
source-hashing idea). See [41-five-tuple-pool-dispatch.md](41-five-tuple-pool-dispatch.md)
for the full design and the rationale for stateless hashing over conntrack.

```go
func flowHash(key flowKey, dstIP net.IP, slots int) int {
    if !key.hasPorts {
        return ipHash(dstIP, slots)   // ICMP / fragments / unparsed → legacy dst-IP hash
    }
    var h uint32 = 2166136261  // FNV offset basis
    // mix proto, dstIP bytes, srcPort, dstPort (FNV prime 16777619)
    ...
    return int(h % uint32(slots))
}
```

**Properties:**
- Deterministic: same five-tuple → same slot (per-flow consistency)
- Well-distributed: spreads many flows to one hot dst IP evenly across slots
- Zero-allocation: operates on the raw IP byte slice

### Why This Works

1. **Same-flow ordering**: Default traffic uses slot 0. With the multi-slot test override, deterministic hashing retains slot affinity to avoid reordering; it is not a stack-correctness requirement.

2. **Independent reconnect**: Data and control slots manage their own connection lifecycles. Losing the sole data conn temporarily interrupts data routing until it reconnects and registers.

3. **Session continuity**: The server shares one gvisor stack per client across that client's conns. Its grace period retains TCP state across brief transport gaps; this does not depend on four slots. See [48-shared-server-gvisor-stack.md](48-shared-server-gvisor-stack.md).

4. **Shared response path**: Data responses reach the same `tunOutbound` channel and TUN device. RouteHub chooses the first writable registered conn with fallback, not per-flow load balancing.

### Configuration

The default is one data slot plus one control slot. The private `poolSize` is a data-only,
nil-stats test override, not a user-facing setting; no CLI or environment flag is added.

### Tradeoffs

Single-slot dispatch avoids slot-selection parsing/hashing and reduces tunnel connections
while preserving batching and concurrent business flows. It removes spare data-slot fallback
in steady state; neither the old four-slot topology nor the new default eliminates head-of-line
blocking in the shared outer SSH/port-forward TCP transport. No throughput gain is claimed here.

## Buffer Memory Layout

Buffers are obtained from `sync.Pool` (64KB each) and laid out to minimize copies:

```
Buffer layout for outbound packets (client → server):

Offset:  0    1    2    3                    3+n
         ├────┼────┼────┼─────────────────────┤
         │ LH │ LH │ PFX│    IP Packet        │
         │[0] │[1] │[2] │    [3 : 3+n]        │
         └────┴────┴────┴─────────────────────┘
              ▲         ▲
              │         └── TUN reads IP packet here (offset 3)
              └──────────── writeToConn writes length header here (in-place, no copy)

LH  = Length Header (2 bytes, big-endian)
PFX = Prefix byte (1=inject, 0=direct)
```

**Zero-copy optimization**: TUN reads at offset 3, leaving room for the 2-byte datagram header + 1-byte prefix. When writing to TCP, the header is filled in-place at offset 0 — no buffer reallocation or `copy()` needed.

## Performance Optimizations

### Per-Packet Hot Path

| Optimization | Before | After | Impact |
|---|---|---|---|
| IP parsing | `ipv4.ParseHeader()` (allocates struct) | `ParseIPFast()` (direct byte slice read) | 0 allocs/packet |
| Route lookup key | `net.IP.String()` (allocates string) | `string(net.IP)` (compiler-optimized) | 0 allocs/packet |
| Debug logging | Always evaluates args | `if config.Debug { ... }` | 0 allocs in production |
| Buffer write | Copy to new buffer + write header | In-place header at reserved headroom | 0 extra copies |

### Channel Buffer Sizes

```go
MaxSize = 1000  // tunInbound, tunOutbound, per-slot channels
```

### Keepalive & Timeouts

| Parameter | Value | Purpose |
|---|---|---|
| `KeepAliveTime` | 60s | Heartbeat interval |
| Read deadline | 180s (3x keepalive) | Detect dead connections |
| Write deadline | 60s | Detect blocked writes |
| Reconnect backoff | 2s | Avoid thundering herd on failure |

## Multi-Client Routing (Server-Side)

When multiple kubevpn clients connect to the same traffic manager, the server
maintains a `RouteHub` — a thread-safe routing table that maps client TUN IPs
to their connection(s).

### RouteHub and ConnList

Each client IP maps to a `ConnList` — a list of TCP connections for that client,
ordered **newest-first**: `AddRoute` prepends, so the most recently established
conn leads. Writes try the head first, so after a reconnect reverse traffic flows
on the fresh conn immediately and any lingering half-open "ghost" conn sinks to
the tail (where it is a last resort, and is evicted by the read/write deadlines —
see [Write with Fallback](#write-with-fallback)). The current default has one registered
data conn per client address in steady state; the control conn is excluded. Multiple entries
can still occur during reconnect overlap or with a multi-slot test override.
Multiple clients coexist in the same table:

```
Client A (198.18.0.2):  1 registered data conn
Client B (198.18.0.3):  1 registered data conn

RouteHub.routes (private, guarded by hub.mu):
  198.18.0.2 → ConnList[ conn_A0 ]
  198.18.0.3 → ConnList[ conn_B0 ]
```

### Route Registration

Routes are registered when the server receives a packet from a client connection:
it calls `hub.AddRoute(ctx, srcIP, conn)`. Duplicate connections are ignored (dedup by
pointer equality); a closed `bufferedTCP` is rejected and its reader must exit. Control
connections are excluded. Registration comes from:

- **Proactive registration** — a freshly dialed slot writes a registration packet
  (an ICMP echo to the gateway, carrying its TUN IP) on the new conn *before* any
  data, so the server registers the route immediately. This is sent directly on the
  conn (not via the heartbeat broadcast channel), so it cannot be dropped under load.
- **Business traffic** — outbound packets on a data conn also register its source IP.
  Periodic heartbeat requests use only the control conn and do not register a route;
  their replies depend on the data conn's registration.

### Write with Fallback

The packet path uses `WriteToRoutePacket(ctx, dstKey, packet)`. It snapshots the current
`ConnList` and its member identities, then revalidates the list and member before I/O.
Failed-member removal checks those same identities, so an old write cannot remove a
re-added connection or a replacement list. Neither `hub.mu` nor `ConnList.mu` is held
across socket writes, queue admission, or `Close`.

The following illustrates fallback when multiple entries exist (reconnect overlap or a
multi-slot test override), not the normal single-data-conn topology:

```
WriteToRoutePacket(ctx, dstKey, packet):
  try conn_B0 → enqueue/write succeeds → return conn_B0, nil

  or: conn_B0 fails before any frame bytes → remove that member
      try conn_B1 → enqueue/write succeeds → return conn_B1, nil

  or: conn_B0 partially writes then fails → return error, never replay on conn_B1
```

Buffered enqueue cancellation returns without evicting a healthy conn or falling back.
Raw writes use a separate context-aware serialization gate: cancellation before a write
starts leaves the conn intact; cancellation during active socket I/O closes the selected
conn, waits for that close callback, and removes only the selected membership. Positive
short writes continue on the same conn through `writeFull`, not on the next route.
Legacy `WriteFuncToRoute` callbacks do not report byte progress or promise cancellable I/O;
they are not the framed-packet routing path.

This is first-writable-conn fallback, not per-flow load balancing or a packet-loss
guarantee. With the default single data conn there is no spare registered data slot;
routing resumes after reconnect and registration.

The fallback walk also covers the **ghost conn** hazard: a write to a half-open
TCP succeeds into the kernel send buffer (no error) until that buffer fills, so a
dead-but-undetected conn could silently swallow reverse traffic. Two defenses bound
this: writes prefer the newest conn (the ghost is at the tail, rarely tried), and a
ghost is actively evicted by the deadlines below within `KeepAliveTime` (write) /
`KeepAliveTime*3` (read).

### Cross-Client Data Flow (A→B)

This multi-entry fallback example applies to reconnect overlap or a multi-slot test override;
the default steady-state path has only `conn_A0` and `conn_B0`.

```
1. Client A sends packet with dst=198.18.0.3
2. Server receives on one of A's conns
3. Server: AddRoute(ctx, srcIP, conn_A0)                 ← register A's route
4. Server: HasRoute(dstKey)                              ← B has a data route
5. Server: WriteToRoutePacket(ctx, dstKey, packet)        ← validated snapshot of B's conns
6. If conn_B0 fails with zero progress → remove member, try conn_B1
   Partial frame failure or cancellation → stop; no fallback/resend
7. Client B receives a successfully routed packet and writes it to TUN
```

### Cleanup

When a connection closes, `RemoveRoutesByConn(ctx, conn)` removes it from every
`ConnList` under `hub.mu`. Empty-route deletion and the internal `emptyLocked` callback
are serialized with registration and `addedLocked`. Callback lock order is `hub.mu` then
`handler.mu`; callbacks only change metadata/timers, with no I/O, logging, or hub reentry.

This cleanup is driven by the server-side conn deadlines so that even a half-open
ghost (no FIN/RST) is reclaimed: the read loop (`readFromTCPConnWriteToEndpoint`)
times out after `KeepAliveTime*3` of silence, and the buffered writer
(`bufferedTCP.run`) times out a stuck write after `KeepAliveTime`; either closes the
conn. The buffered writer's stop callback removes its routes immediately; reader cleanup
also removes routes idempotently. `AddRoute` refuses that stopped writer, so late reads
cannot restore it. See [08-heartbeat-health.md](08-heartbeat-health.md) §#3.

There is no separate public-map `IsEmpty`/`Delete` transaction: the private table, validated
snapshots, and identity-safe removal prevent stale writers from deleting restored routes.
The private `tunQueue` bridge admits packets through cancellable `enqueueTUN`; shutdown
signals `tunDone`, joins in-flight admission, then drains without closing the data channel.
Server device sends are also cancellable. See [12-session-lifecycle.md](12-session-lifecycle.md)
for server-device failure and joined shutdown.

## MTU Calculation

```
MTU = 1500 (Ethernet)
    - 40   (IPv6 header, worst case)
    - 20   (TCP header)
    - 22   (TLS 1.3: 5 header + 1 content type + 16 auth tag)
    - 3    (Datagram framing: 2 length + 1 prefix)
    = 1415 bytes
```

This ensures a full-MTU TUN packet fits within a single Ethernet frame after all encapsulation layers.

## Data-Plane Package Boundaries

The data-plane stack (`pkg/core`, `pkg/dns`) depends only on network primitives — it must not
pull in Kubernetes cluster-API, Helm, or Docker transitive dependencies. This is enforced by the
`pkg/util/netutil` sub-package (C3 refactoring):

| Package | Contents | Imports |
|---------|----------|---------|
| `pkg/util/netutil` | Pure transport/packet/TLS/gvisor primitives: `SafeWrite`/`SafeClose`, `HandleCrash`, `ParseIPFast`, `IsIPv6Enabled`, TUN device helpers, ICMP generation, `WriteProxyInfo`/`ParseProxyInfo`, TLS helpers | Only `pkg/config` and `pkg/log` from the kubevpn module |
| `pkg/util` | K8s cluster-ops, Docker, Helm, file/upgrade helpers | K8s client-go, Helm, Docker, CNI libs, … |

`pkg/core` and `pkg/dns` import `pkg/util/netutil` and have **no dependency on `pkg/util`**.
This keeps the data-plane binary free of the heavy transitive closure that cluster-ops libraries
bring. All 19 cmd/ files and all upper-layer packages (`pkg/handler`, `pkg/inject`, `pkg/run`,
`pkg/daemon/action`) continue to import `pkg/util` directly — they were already at the top of
the import graph and gain nothing from the split.
