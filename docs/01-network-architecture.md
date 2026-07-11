# KubeVPN Network Architecture

## Overview

KubeVPN creates a bidirectional network tunnel between a local machine and a Kubernetes cluster. All IP traffic (TCP, UDP, ICMP) flows through a TUN device on the client, gets encapsulated over TCP, and is forwarded via `kubectl port-forward` to a traffic manager pod running a userspace network stack (gvisor).

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

IP packets are encapsulated in a custom datagram framing protocol over TLS/TCP:

```
┌──────────────────────────────────────────┐
│         Datagram Frame (on TCP)          │
├──────────┬────────┬──────────────────────┤
│ Length   │ Prefix │      IP Packet       │
│ (2 bytes)│(1 byte)│   (variable length)  │
├──────────┼────────┼──────────────────────┤
│ uint16   │ 0 or 1 │  IPv4/IPv6 packet    │
│ big-end  │        │                      │
└──────────┴────────┴──────────────────────┘
```

- **Length** (2 bytes): big-endian uint16, value = len(prefix) + len(IP packet)
- **Prefix** (1 byte): routing directive (see below)
- **IP Packet**: raw IPv4 or IPv6 packet

### Prefix Byte Semantics

| Value | Name | Direction | Meaning |
|-------|------|-----------|---------|
| `1` | INJECT | client→server | Inject into gvisor stack for TCP/UDP processing |
| `1` | INJECT | server→client | Inject into client gvisor stack (rare: self-to-self traffic) |
| `0` | DIRECT | server→client | Already processed by gvisor; write directly to TUN device |
| `0` | FORWARD | server internal | Forward to another client via RouteHub (multi-client routing) |

**Routing decision on server (per packet):**
```go
if buf[0] == 1 {
    // Inject into gvisor stack → TCP/UDP forwarder → dial real service
    endpoint.InjectInbound(protocol, pkt)
} else {
    // Forward via RouteHub TCPPacketChan → server TUN → other client
    hub.TCPPacketChan <- packet
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

The client maintains a pool of N parallel TCP connections to reduce head-of-line blocking:

```
                        ┌── slot[0] ──▶ conn[0] ──▶ port-forward ──▶ gvisor stack[0]
TUN read                │
  ↓                     │
ParseIPFast(dst) ───────┼── slot[1] ──▶ conn[1] ──▶ port-forward ──▶ gvisor stack[1]
  ↓                     │
hash(dst IP) % N ───────┼── slot[2] ──▶ conn[2] ──▶ port-forward ──▶ gvisor stack[2]
                        │
                        └── slot[3] ──▶ conn[3] ──▶ port-forward ──▶ gvisor stack[3]


Response path (all slots → shared channel → single TUN device):

conn[0] ──▶ readFromConn ──┐
conn[1] ──▶ readFromConn ──┤──▶ tunOutbound ──▶ writeToTun ──▶ TUN device
conn[2] ──▶ readFromConn ──┤
conn[3] ──▶ readFromConn ──┘
```

### Hash Function

FNV-1a hash of destination IP bytes, mod N:

```go
func ipHash(ip net.IP, slots int) int {
    var h uint32 = 2166136261  // FNV offset basis
    for _, b := range ip {
        h ^= uint32(b)
        h *= 16777619  // FNV prime
    }
    return int(h % uint32(slots))
}
```

**Properties:**
- Deterministic: same dst IP → same slot (flow consistency)
- Well-distributed: spreads traffic evenly across slots
- Zero-allocation: operates on raw IP byte slice

### Why This Works

1. **Same-flow guarantee**: All packets to the same destination IP hash to the same slot → same TCP connection → same server-side gvisor stack. The gvisor stack correctly tracks TCP connection state.

2. **Independent reconnect**: Each slot manages its own connection lifecycle. If slot[2] disconnects, only 1/4 of traffic is affected while it reconnects.

3. **Server compatibility**: The server's accept loop (`go svr.Handler.Handle(ctx, conn)`) already handles multiple connections independently. Each connection gets its own gvisor stack — functionally correct, with acceptable memory overhead.

4. **Shared response path**: All slots write responses to the same `tunOutbound` channel → same TUN device. Response packets arrive correctly regardless of which slot they came through.

### Configuration

```go
const ConnPoolSize = 4  // pkg/core/tun_client.go
```

### Tradeoffs

| Benefit | Cost |
|---------|------|
| 4x reduction in HOL blocking | 4x gvisor stacks on server (memory) |
| Better bandwidth utilization | 4x port-forward goroutines |
| Fault isolation (1 slot failure = 25% impact) | Slightly more complex client code |
| Parallelism across CPU cores | — |

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

Each client IP maps to a `ConnList` — an ordered list of TCP connections for
that client. Because the connection pool creates N parallel connections per
client, a single client IP has N entries (one per slot). Multiple clients
coexist in the same table:

```
Client A (198.18.0.2):  4 conns via connection pool
Client B (198.18.0.3):  4 conns via connection pool

RouteHub.routeMapTCP:
  198.18.0.2 → ConnList[ conn_A0, conn_A1, conn_A2, conn_A3 ]
  198.18.0.3 → ConnList[ conn_B0, conn_B1, conn_B2, conn_B3 ]
```

### Route Registration

Routes are registered lazily: when the server receives a packet from a client
connection, it calls `hub.AddRoute(srcIP, conn)`. Duplicate connections are
ignored (dedup by pointer equality). Heartbeat ICMP packets ensure registration
happens before any real traffic.

### Write with Fallback

When the server needs to forward a packet to a client (e.g., cross-client
traffic A→B, or proxy response), it uses `WriteToRoute` or `WriteFuncToRoute`:

```
WriteFuncToRoute("198.18.0.3", dgram.Write):
  try conn_B0 → write succeeds → return conn_B0, nil
  
WriteFuncToRoute("198.18.0.3", dgram.Write):
  try conn_B0 → write fails (dead conn) → remove conn_B0
  try conn_B1 → write succeeds → return conn_B1, nil
```

This eliminates packet loss during connection pool slot reconnections: when
one slot dies and reconnects, the remaining 3 slots handle traffic seamlessly.

### Cross-Client Data Flow (A→B)

```
1. Client A sends packet with dst=198.18.0.3
2. Server receives on one of A's conns
3. Server: AddRoute("198.18.0.2", conn_A_slot)         ← register A's route
4. Server: HasRoute("198.18.0.3")                      ← true, B is connected
5. Server: WriteFuncToRoute("198.18.0.3", dgram.Write) ← try B's conns in order
6. If conn_B0 dead → removed, tries conn_B1 → success
7. Client B receives packet on conn_B1, writes to TUN
```

### Cleanup

When a connection closes, `RemoveRoutesByConn(conn)` iterates all route entries
and removes that connection from every `ConnList` it appears in. If a `ConnList`
becomes empty, the route entry is deleted entirely.

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
