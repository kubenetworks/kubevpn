# Five-Tuple Connection-Pool Dispatch

## Overview

The client maintains a pool of `ConnPoolSize = 4` parallel TCP connections to the server
(see [01-network-architecture.md](01-network-architecture.md) and
[18-gvisor-network-stack.md](18-gvisor-network-stack.md)). Each outbound packet read from
the TUN is assigned to one slot. This document describes how that assignment is made: a
**stateless five-tuple hash**, modeled on IPVS source-hashing (`sh`) / Maglev rather than
iptables/nf_conntrack's stateful flow table.

It supersedes the earlier dst-IP-only scheme (`ipHash(dst)`) documented in §"Connection
Pool Architecture" of [01-network-architecture.md](01-network-architecture.md).

## The Problem It Solves

The previous dispatcher hashed **only the destination IP**:

```go
slot := ipHash(packet.dst, n)   // FNV-1a over dst IP bytes, mod N
```

This pins *every* flow to a given destination IP onto a single slot, regardless of the
local source port. In real development traffic the destinations are few and hot — one API
gateway, one database — so all concurrent connections to that one service IP collapse onto
**one** connection. The 4-connection pool degenerates to a single connection, and its whole
reason for existing (reducing head-of-line blocking, parallelizing throughput) is lost.

Because the server runs an independent gvisor stack per pool connection and the return path
is symmetric (below), the down-link concentrates on that same slot too — so neither
direction benefits from the pool for a hot destination.

## Design: Stateless Five-Tuple Hash

Dispatch now hashes the flow's five-tuple — `(proto, dst IP, src port, dst port)` — so that
distinct connections to the same hot destination IP (which differ in source port) spread
across the pool:

```
TUN read
  ↓
parseFiveTupleInline(IP)         → flowKey{proto, srcPort, dstPort, hasPorts}
  ↓
flowHash(flowKey, dst IP) % N    → slot index
  ↓
trySendToSlot(slots[slot], pkt)
```

The **source IP is deliberately omitted** from the hash: a client has a single TUN IP, so
it contributes nothing to the distribution. This mirrors IPVS source-hashing, which keys
only on the fields that actually discriminate flows. The source *port*, assigned per
connection by the local stack, is the field that spreads many connections to one dst IP.

### Per-Flow Slot Affinity

Slot assignment is deterministic per flow — every packet of one L4 flow maps to the same
slot. With the shared server-side gvisor stack
([48-shared-server-gvisor-stack.md](48-shared-server-gvisor-stack.md)), this is no longer a
**correctness** requirement (all slots feed the same stack, so a split flow would still
reach the right TCP state), but it is retained for **session affinity** and **avoiding
packet reordering** across slots.

A pure function of the five-tuple satisfies this for free: same tuple → same hash → same
slot. And because `N` is fixed (a dead slot reconnects in place and is never removed from
the array — see [22-tun-device.md](22-tun-device.md)), `hash % N` never changes underneath
a live flow. No rehash, no flow migration.

### The Return Path — RouteHub Routing

With the shared server-side gvisor stack, replies from the stack are routed via RouteHub by
destination IP (the client's TUN IP). `readFromEndpointWriteToRoute` reads each response
packet, parses its destination, and writes it to the first healthy conn in the client's
ConnList — the slot is chosen by availability, not by five-tuple affinity. All of a client's
slots deliver to the same TUN device, so the OS TCP stack handles any reordering.

This is the same routing mechanism already used for inter-client traffic and the native-TUN
path. See [48-shared-server-gvisor-stack.md](48-shared-server-gvisor-stack.md).

## Parsing the Five-Tuple

`parseFiveTupleInline(ipData []byte) flowKey` reads the L4 protocol and ports directly from
the raw IP bytes — zero allocation, returning a value type. It assumes the packet already
passed `ParseIPFast` (sane version/length) but still bounds-checks every offset; a malformed
packet just yields `hasPorts=false`.

```go
type flowKey struct {
    proto    uint8
    srcPort  uint16
    dstPort  uint16
    hasPorts bool   // false → flowHash falls back to ipHash(dst)
}
```

| Case | Handling |
|---|---|
| IPv4, TCP/UDP/SCTP, not fragmented | Read ports at `ipData[IHL*4 : +4]` (honors IP options) |
| IPv4 fragment (MF set **or** frag-offset ≠ 0) | `hasPorts=false` — only the first fragment carries the L4 header |
| IPv6, TCP/UDP/SCTP, no extension header | Read ports at `ipData[40 : 44]` |
| IPv6 with extension header (next-header ≠ 6/17/132) | `hasPorts=false` — extension chains are not walked (rare in practice) |
| ICMP / any other protocol | `hasPorts=false` |
| Truncated L4 header | `hasPorts=false` |

## flowHash and Fallback

```go
func flowHash(key flowKey, dstIP net.IP, slots int) int {
    if !key.hasPorts {
        return ipHash(dstIP, slots)   // legacy behavior, preserved
    }
    // FNV-1a over proto, dstIP bytes, srcPort, dstPort
    ...
    return int(h % uint32(slots))
}
```

When the packet carries no usable ports (fragment, ICMP, unparsed header), `flowHash` falls
back to `ipHash(dstIP)`. This is exactly the old behavior, and it has a useful property for
fragments: all fragments of one datagram share the same destination IP, so they all fall
back to the **same** slot — a fragmented datagram is never split across stacks. This is the
same choice IPVS makes for non-first fragments.

## Why Stateless, Not Conntrack

iptables/nf_conntrack keeps a stateful flow table: the first packet of a flow creates an
entry, later packets look it up. Its value is (a) NAT-mapping consistency and (b) keeping an
established flow pinned to its backend when the backend set changes.

Neither applies here. There is no NAT, and the slot set is **fixed** (`N = 4`, dead slots
reconnect in place). With a fixed `N`, `hash % N` is already perfectly stable per flow — the
hash *is* the state. A flow table would add memory, expiry/GC, and a lookup lock on the
per-packet hot path for zero benefit. So the design is deliberately stateless, in the spirit
of Maglev/IPVS-`sh`.

## Scope of Change

| File | Change |
|---|---|
| `pkg/core/flow_hash.go` | `flowKey`, `parseFiveTupleInline`, `l4Ports`, `flowHash`, `ipHash` (the five-tuple parsing/hashing module, split out of `tun_client.go`). `runConnPool` dispatch uses `flowHash`; `ipHash` is the fallback. |
| `pkg/core/conn_slot.go` | `connSlot` (single-connection read/write goroutines), `periodicDeadline`, `trySendToSlot`, `broadcastToSlots` (split out of `tun_client.go`). |
| `pkg/core/tun_client.go` | `clientTransport` connection-pool orchestration (`runConnPool`, `routeOutbound`, `heartbeats`, `registrationPayloads`). |
| `pkg/core/flow_hash_test.go` | New tests (below). |

Server side, `RouteHub`, `ConnList`, `ParseIPFast`, the `Packet` type, and `pumpTun` are
**unchanged**.

## Tests

`pkg/core/flow_hash_test.go`:

- **Parsing** — TCP/UDP/SCTP ports, IPv4 variable IHL (options), IPv6, and every fallback
  case (ICMP, MF fragment, offset fragment, truncated header, IPv6 extension header).
- **Determinism** — the same five-tuple always maps to the same slot (the affinity
  constraint).
- **Spread** — 256 flows to one dst IP with distinct source ports fill all 4 slots, whereas
  `ipHash(dst)` would pin them to one.
- **Fallback equivalence** — fragments and ICMP hash identically to `ipHash(dst)`.
- **Real-pool integration** (`TestFlowHash_RealPool_Spreads`) — drives the actual
  `runConnPool` over a mock forwarder handing out tracked conns, pushes 64 flows (3 packets
  each) to one dst IP, and asserts they spread across ≥2 pool conns while each individual
  flow stays on exactly one conn.

See [18-gvisor-network-stack.md](18-gvisor-network-stack.md) for the surrounding pool and
gvisor-stack architecture, and [08-heartbeat-health.md](08-heartbeat-health.md) for the
heartbeat/route-liveness that keeps each slot's conn registered.
