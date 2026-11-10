# KubeVPN Network Architecture

## Overview

Current source boundaries: `pkg/network/runtime.go` coordinates host networking;
`transport.go` owns serial recovery, `transport_generation.go` owns per-generation resources,
`transport_prepare.go` builds SSH/port-forward endpoints, `transport_health.go` evaluates
current-generation liveness, and `transport_policy.go` owns retry state; `address.go` owns allocation/IP watches,
`routes.go` owns route effects, `dns.go` owns DNS setup, `device.go` assembles the TUN,
and `monitor.go` observes underlay changes (all under `pkg/network`). Kubernetes forwarding
sessions live in `pkg/cluster/portforward`; CIDR detection lives in `pkg/cluster/discovery`.
`pkg/tunnel` owns packet/device/slot/router lifecycles, `pkg/tunnel/internal/netstack`
owns gvisor construction and forwarders, and `pkg/internal/wire` owns stream framing and
batch writes. `pkg/endpoint` retains protocol/URI/TLS/SSH/UDP-relay assembly and `Serve`.

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
prefix within that length. `wire.ReadDatagram` reads the two-byte header into independent
storage, then reads the body into the caller's buffer. An undersized buffer returns
`io.ErrShortBuffer`; the consumed header makes the stream unusable, so the framing wrapper
closes it rather than trying to resume. Outbound payloads above 65535 are rejected before
any write. Specifically, `wire.Conn.Write` uses standalone storage for its largest
legal frame (65537 bytes), without returning that storage to `config.LPool`. This is the
wrapper's allocation contract, not a guarantee for every framed-packet path. The uint16
wire limit is unchanged; other pooled paths can reject packets when their buffer is too short.

`WriteFull` continues positive short writes on the **same connection**, reports actual wire
progress, and preserves errors (zero progress without an error becomes `io.ErrShortWrite`).
A partially written frame is never replayed on another connection. Read and Write are
serialized separately by `wire.Conn`; context cancellation/Close closes the transport
without waiting for an I/O mutex. These are safety contracts, not a wire-format change.

### Prefix Byte Semantics

| Value | Name | Direction | Meaning |
|-------|------|-----------|---------|
| `1` | INJECT | client→server | Inject into gvisor stack for TCP/UDP processing |
| `1` | INJECT | server→client | Inject into client gvisor stack (rare: self-to-self traffic) |
| `0` | DIRECT | server→client | Already processed by gvisor; write directly to TUN device |
| `0` | FORWARD | server internal | Forward to another client via Router (multi-client routing) |
| `2` | CONTROL | client→server | Heartbeat control conn; handled separately, never registered in Router |

**Routing decision on server (data conn, per packet; control conns use `handleControlConn`):**
```go
// Simplified: registered peer destinations route directly before prefix handling.
if hub.HasRoute(dstKey) {
    _, err = hub.WriteToRoutePacket(ctx, dstKey, packet)
    packet.release() // caller's reference
} else if prefix == packetTypeToGvisor {
    // Inject into the per-client stack → TCP/UDP forwarder → real service.
    endpoint.InjectInbound(protocol, pkt)
} else if !hub.bridge.send(ctx, packet) {
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

Data response path:
per-client gvisor stack → routeWriter → registered data conn
  → packetReader → dispatchReadPacket → tunOutbound → writeToTun → TUN device

Heartbeat reply path:
handleControlConn → synthesizeEchoReply on the SAME control conn
  → packetReader → HeartbeatStats.observeReply (consumed, not written to TUN)
```

Protocol-owned heartbeat replies are consumed by the client rather than written to TUN.
Outbound dispatch goes directly to slot 0, without parsing or hashing for slot
selection. `connSlot` (`pkg/tunnel/slot.go`) retains its queue and retry lifecycle;
`connAttempt` (`pkg/tunnel/attempt.go`) owns one connection, declaration/registration,
`readGeneration`/`writeGeneration`, deadlines and batching, and joins both pumps before retry. One tunnel data connection still multiplexes concurrent business flows;
it does not serialize application requests or reduce business concurrency to one.

The control slot has a separate queue and connection to the **same TCP forwarding endpoint
10801**, not xDS port 9002. Its requests bypass the business queue, and the server answers
them inline on that same control conn. SSH/port-forward still shares an outer TCP transport,
so separation is not an absolute liveness guarantee.

Earlier revisions also kept a five-tuple hash multi-slot dispatch as a test/benchmark-only
override; it was removed as a production-dead path. The historical design is recorded in
[41-five-tuple-pool-dispatch.md](41-five-tuple-pool-dispatch.md).

### Why This Works

1. **Same-flow ordering**: All default traffic uses slot 0, so per-flow packet ordering is trivially preserved.

2. **Independent reconnect**: Data and control slots manage their own connection lifecycles. Losing the sole data conn temporarily interrupts data routing until it reconnects and registers.

3. **Session continuity**: The server shares one gvisor stack per client across that client's conns. Its grace period retains TCP state across brief transport gaps; this does not depend on four slots. See [48-shared-server-gvisor-stack.md](48-shared-server-gvisor-stack.md).

4. **Shared response path**: Data responses reach the same `tunOutbound` channel and TUN device. Router chooses the first writable registered conn with fallback, not per-flow load balancing.

### Configuration

One data slot plus one control slot is the only topology; there is no pool-size
setting, flag, or test override.

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
              └──────────── wire.BatchWriter stamps length here (in-place, no copy)

LH  = Length Header (2 bytes, big-endian)
PFX = Prefix byte (1=inject, 0=direct)
```

**Zero-copy optimization**: TUN reads at offset 3, leaving room for the 2-byte datagram header + 1-byte prefix. The shared `wire.BatchWriter` validates and stamps the header in place immediately before socket I/O — no buffer reallocation or `copy()` needed.

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
MaxSize = 256  // tunInbound, tunOutbound, per-slot channels
```

### Keepalive & Timeouts

| Parameter | Value | Purpose |
|---|---|---|
| `HeartbeatInterval` | 5s | Correlated control-heartbeat interval |
| `KeepAliveTime` | 60s | Write deadline and peer grace base |
| Read deadline | 180s (3x keepalive) | Detect dead connections |
| Write deadline | 60s | Detect blocked writes |
| Reconnect backoff | 2s | Avoid thundering herd on failure |

## Multi-Client Routing (Server-Side)

When multiple kubevpn clients connect to the same traffic manager, the server
maintains a `Router` facade over one `serverRuntime`. Its `routeTable` maps client
TUN IPs to connection lists, `routeWriter` owns fallback and failure reconciliation,
and the same runtime explicitly shares `peerRegistry` and `tunBridge`.

### Router and ConnList

Each client IP maps to a `ConnList` — a list of TCP connections for that client,
ordered **newest-first**: `AddRoute` prepends, so the most recently established
conn leads. Writes try the head first, so after a reconnect reverse traffic flows
on the fresh conn immediately and any lingering half-open "ghost" conn sinks to
the tail (where it is a last resort, and is evicted by the read/write deadlines —
see [Write with Fallback](#write-with-fallback)). The current default has one registered
data conn per client address in steady state; the control conn is excluded. Multiple entries
can still occur during reconnect overlap.
Multiple clients coexist in the same table:

```
Client A (198.18.0.2):  1 registered data conn
Client B (198.18.0.3):  1 registered data conn

Router.routeTable.routes (private, guarded by routeTable.mu):
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
  their replies return inline on that conn. Registration is still required for reverse
  data traffic and inter-client responses.

### Write with Fallback

The packet path uses `routeWriter.writeToRoutePacket` through the public
`Router.WriteToRoutePacket` facade. It snapshots the current
`ConnList` and its member identities, then revalidates the list and member before I/O.
Failed-member removal checks those same identities, so an old write cannot remove a
re-added connection or a replacement list. Neither `routeTable.mu` nor `ConnList.mu` is held
across socket writes, queue admission, or `Close`.

The following illustrates fallback when multiple entries exist (reconnect overlap),
not the normal single-data-conn topology:

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
short writes continue on the same conn through `WriteFull`, not on the next route.
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

This multi-entry fallback example applies to reconnect overlap;
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
`ConnList` under `routeTable.mu`. Empty-route deletion and registration call the explicit
`peerRegistry.onRouteEmptyLocked` / `onRouteAddedLocked` methods in the same transaction.
Lock order is router then registry; transitions only change metadata/timers, with no I/O,
logging, or router reentry. `endpoint.Serve` starts that registry with the service context
before accept and closes/joins it after connection handlers finish; no connection owns peers.

This cleanup is driven by the server-side conn deadlines so that even a half-open
ghost (no FIN/RST) is reclaimed: the read loop (`readFromTCPConnWriteToEndpoint`)
times out after `KeepAliveTime*3` of silence, and the buffered writer
(`bufferedTCP.run`) times out a stuck write after `KeepAliveTime`; either closes the
conn. The buffered writer's stop callback removes its routes immediately; reader cleanup
also removes routes idempotently. `AddRoute` refuses that stopped writer, so late reads
cannot restore it. See [08-heartbeat-health.md](08-heartbeat-health.md) §#3.

There is no separate public-map `IsEmpty`/`Delete` transaction: the private table, validated
snapshots, and identity-safe removal prevent stale writers from deleting restored routes.
The separate `tunBridge` (`pkg/tunnel/bridge.go`) admits packets through cancellable
`send`; `stop` signals shutdown, joins in-flight admission, then drains without closing
the data channel. Connection handlers also join their `bufferedTCP` writer via `waitRun`.
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

The data-plane stack (`pkg/tunnel`, its `internal/netstack`, `pkg/internal/wire`, and `pkg/dns`) depends only on network primitives — it must not
pull in Kubernetes cluster-API, Helm, or Docker transitive dependencies. This is enforced by the
`pkg/util/netutil` sub-package (C3 refactoring):

| Package | Contents | Imports |
|---------|----------|---------|
| `pkg/util/netutil` | Pure transport/packet/TLS/gvisor primitives: `SafeWrite`/`SafeClose`, `HandleCrash`, `ParseIPFast`, `IsIPv6Enabled`, TUN device helpers, ICMP generation, `WriteProxyInfo`/`ParseProxyInfo`, TLS helpers | Only `pkg/config` and `pkg/log` from the kubevpn module |
| `pkg/util` | K8s cluster-ops, Docker, Helm, file/upgrade helpers | K8s client-go, Helm, Docker, CNI libs, … |

Packet/stack code uses `pkg/util/netutil`, not the cluster helpers in `pkg/util`.
`pkg/network` is the host-networking orchestrator and deliberately consumes Kubernetes
clients plus `pkg/cluster/portforward` and `pkg/cluster/discovery`; it is not part of the
cluster-independent packet layer. `pkg/endpoint` remains the assembly/Serve boundary.
