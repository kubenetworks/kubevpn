# KubeVPN Core Data Flow Analysis

## Overview

The data-plane implementation now lives in `pkg/tunnel`: `client.go`, `slot.go`,
`attempt.go`, `server.go`, `peers.go`, `route.go`, `device.go`, `bridge.go`,
`packet.go`, `batch.go` and `heartbeat.go`. Historical filenames below describe
those same paths before extraction. `pkg/internal/wire` owns shared stream framing
and batch socket writes; `tunnel/internal/netstack` owns stack construction and
TCP/UDP/ICMP forwarders. `endpoint` only assembles listeners/dialers and runs `Serve`.

KubeVPN uses a TUN device + gvisor network stack between a local client and a remote
Kubernetes cluster. The current default multiplexes business flows on one data connection
plus a separate control connection to the same forwarding endpoint.

### Current ownership and data paths

- Root assembly: `pkg/network/runtime.go` → `pkg/network/device.go` → tunnel handlers;
  `pkg/network/transport.go` owns SSH/port-forward generations. Transport replacement
  retains the TUN and client stacks; `address.go` applies legitimate lease changes.
- Client: `pkg/tunnel/client.go` dispatches directly to data slot 0. `slot.go` retains
  queues/retry state, while `attempt.go` creates a `connAttempt` per dial, owns framing
  declaration/registration plus `readGeneration`/`writeGeneration`, and joins both pumps
  before retry. Multi-slot hashing is only a private nil-stats test override.
- Server: `pkg/tunnel/route.go` defines `Router` with private route membership,
  `peerRegistry` and `tunBridge`, not public `RouteMapTCP`/`TCPPacketChan` globals.
  `server_endpoint.go` sends per-client stack output through `readFromEndpointWriteToRoute`
  → Router → registered data conn. There is no direct per-connection stack reply writer.
- Service lifetime: `endpoint.Serve` (`pkg/endpoint/serve.go`) calls handler `Start(serviceCtx)`
  before accept, joins accepted handlers, then calls `Close` to join peer construction,
  output and destruction. `peerRegistry` never borrows the first connection's context.
  `bridge.go` separately owns queue admission/stop/drain. Connection handlers close and
  join their buffered writer through `waitRun`; cancellation alone is not a join.
- Heartbeat: `HeartbeatInterval` (5 s) → control queue/conn → per-client stack →
  **registered data conn** → correlated `HeartbeatStats.observeReply`. Control does not
  register routes and receives no direct reply. Type `2` is control; only `3..255` remain
  reserved. Data registration is sent before queued business traffic on each new data conn.
- Shared primitives: `pkg/internal/wire/conn.go` owns `Conn`, `ReadDatagram`,
  `WriteDatagram`, `WriteFull`; `batch.go` owns `BatchWriter`. Pooled `packet` and packet
  batches remain private to tunnel. `pkg/tunnel/internal/netstack` owns stack construction
  and TCP/UDP/ICMP forwarders.

The legacy diagrams, pseudocode and bug-fix/performance history below preserve earlier
names/topologies (public RouteHub maps, four slots, broadcast heartbeats, old wrapper APIs).
They are explanatory history, not current source declarations or new measurements. Read
[18](../../docs/18-gvisor-network-stack.md) and [48](../../docs/48-shared-server-gvisor-stack.md)
for current routing/lifetime details. Current packet tests run under `go test ./pkg/tunnel/...`;
shared framing tests under `go test ./pkg/internal/wire`. No tests were run in this doc pass.

## Canonical buffer layout

The whole data plane uses a single buffer layout (constants in `packet.go`):

```
data[0:2]  datagram length header  (datagramHeaderLen = 2)
data[2]    type prefix             (typePrefixLen = 1)
data[3:]   raw IP payload          (starts at tunReserve = 3)

Packet.length = typePrefixLen + len(IP)   // type + IP
wire frame    = data[0 : datagramHeaderLen+length]
raw IP        = data[tunReserve : datagramHeaderLen+length]
```

`pumpTun` reads the TUN into `data[3:]`, reserving the 3-byte head so the datagram
length and type prefix can be written in place — no shifting on the way out. See
[Batched / segment-aware TUN reads](#batched--segment-aware-tun-reads) for how one syscall
may deliver several packets (wireguard-go Linux GSO splitting) into a batch of these buffers.

### Type prefix

The 1-byte type prefix at `data[2]` (named constants in `packet.go`); it is a small
extensible discriminator — values `3..255` are reserved for future packet types:

| Prefix | Constant | Meaning | Action at receiver |
|--------|----------|---------|--------------------|
| `0` | `packetTypeToTUN` | Gvisor-processed packet (response from real network) | Write to TUN device directly |
| `1` | `packetTypeToGvisor` | Raw IP packet (needs gvisor processing) | Inject into gvisor stack |
| `2` | `packetTypeControl` | Control declaration / heartbeat ICMP | No route registration; replies use registered data conns |

## Batched / segment-aware TUN reads

The TUN device is a wireguard-go `tun.Device`, whose API reads/writes **multiple packets per
syscall**: `Read(bufs [][]byte, sizes []int, offset int)`. `tunDevice.pumpTun` picks the loop
based on the device:

- **`pumpTunBatch`** (real TUN devices) — retains `BatchSize()` buffer slots across reads,
  refilling only those transferred to dispatch. Unused, invalid and malformed slots stay
  pump-owned. It parses and dispatches the valid reported prefix even when Read also returns
  an error; cancellation instead stops further dispatch and returns all still-owned buffers.
  Ownership transfers before calling dispatch, so retained consumer buffers cannot be reused.
- **`pumpTunSingle`** (plain `net.Conn`, e.g. `net.Pipe` in tests) — the original one-packet
  loop, not the retained-batch optimization.

On Linux, native Read splits kernel **GSO** input into caller-provided buffers (up to 128
packets); it does not GRO-append/reallocate their bases. The adapter removes its extra read
copy only for Linux `*tun.NativeTun` and sufficiently capacious destinations. Linux `LPool`
returns length **65536**, capacity **65538**; temporary aliases of length 65538 permit native
Read at offset 3 to receive a **65535-byte** segment. The adapter then filters against the
unchanged logical IP limit **65533**, compacts valid packets by swapping whole buffers, and
clears the aliases. Oversized packets are dropped, not truncated or put on the wire.

Non-Linux, non-native and insufficient-capacity reads keep full-size scratch and copying.
macOS/Windows have `BatchSize()==1`. Linux write-side **GRO** mutation/coalescing still needs
private scratch, 16-byte headroom and header clearing; this optimization does not change it.
Offload retains wireguard-go's internal read/split work: removing the adapter copy is not
end-to-end zero-copy. See [TUN device design](../../docs/22-tun-device.md) §4.

**Memory is a trade-off:** Darwin-host allocation modeling of the Linux capacities rounds
65538 to 73728 bytes, adding 8192 bytes per distinct Linux pooled buffer, while removing
128 adapter read-scratch allocations saves 9 MiB. With 128 pump buffers plus 256 distinct
queued buffers the model nets -6 MiB; with 2048 queued it nets +8 MiB. Shared references
count once. Darwin's actual pool remains 65536 bytes; this is neither a global memory
reduction claim nor a real Linux-kernel benchmark.

## Datagram Protocol (UDP-over-TCP)

TCP is a stream protocol, so packets are framed with a 2-byte big-endian length header:

```
[2 bytes: payload length][N bytes: payload (type prefix + IP packet)]
```

Read: `readDatagramPacket` reads the 2-byte length, then reads exactly N bytes.
Write: `writeDatagram(w, buf, payloadLen)` stamps the length header in place in the
buffer's reserved head and passes the contiguous frame `buf[:2+payloadLen]` to `writeFull`.
Header stamping is idempotent; **transmission is not safe to retry across connections**.
Positive short writes continue only on the same writer; zero progress or an error terminates
the write, and a partly transmitted frame must not be replayed on another socket. Route
fallback stops once any bytes were written. `frameBatchWriter` preserves each frame's
header while batching and reports the complete-frame prefix, including before an error.

### Reference-counted packet buffers

Pooled buffers (`config.LPool`) are wrapped in `*Packet` with an atomic reference count
(`packet.go`, modeled on gvisor's `buffer.View` chunk refcount). The zero value means a
single owner; `acquire()` adds a reference and `release()` returns the buffer to the pool
when the last reference is dropped (releasing more than acquired panics, turning a
use-after-free into a loud failure). This lets a buffer be handed to an async consumer
(e.g. `bufferedTCP`'s queue) by reference instead of being copied.

---

## Server Data Flow

**Entry point:** `cmd/kubevpn/cmds/server.go`

**Listeners:** typically `tun://?net=198.18.0.x/16` + `gtcp://:10801`

### Architecture Diagram

```
                         K8s Cluster (Server)
┌─────────────────────────────────────────────────────────┐
│                                                         │
│  ┌─────────┐     ┌──────────┐     ┌──────────────────┐ │
│  │   TUN   │────▶│  Device  │────▶│  Peer.routeTun   │─┼──▶ RouteMapTCP lookup
│  │ Device  │◀────│readFromTun│    │                  │ │   ├─ found: send to client
│  │         │     └──────────┘     │routeTCPToTun◀────│─┼── │  via BufferedTCP
│  └─────────┘      tunInbound      └──────────────────┘ │   └─ not found: drop
│       ▲            channel              ▲               │
│       │                                 │               │
│       │          tunOutbound      TCPPacketChan          │
│       │            channel              │               │
│       └─────── writeToTun ◀─────────────┘               │
│                                                         │
│  ┌──────────────────────────────────────────────────┐   │
│  │              gtcp TCP Listener (:10801)           │   │
│  │                                                    │  │
│  │  Per-client connection:                            │  │
│  │  ┌────────────────────────────┐                    │  │
│  │  │ readFromTCPConnWriteToEndpoint                 │  │
│  │  │   ├─ Parse datagram (2-byte header)            │  │
│  │  │   ├─ Update RouteMapTCP[srcIP] = conn          │  │
│  │  │   ├─ if dst in RouteMapTCP → route to client   │  │
│  │  │   ├─ if prefix==1 → inject to gvisor stack     │  │
│  │  │   └─ if prefix==0 → TCPPacketChan → TUN        │  │
│  │  └────────────────────────────┘                    │  │
│  │  ┌────────────────────────────┐                    │  │
│  │  │ readFromEndpointWriteToTCPConn                 │  │
│  │  │   └─ Read gvisor response → prefix=0 → client  │  │
│  │  └────────────────────────────┘                    │  │
│  │                                                    │  │
│  │  ┌──────────────────┐                              │  │
│  │  │  Gvisor Stack    │                              │  │
│  │  │  ├─ TCPForwarder │──▶ dial real k8s service     │  │
│  │  │  ├─ UDPForwarder │──▶ dial real k8s service     │  │
│  │  │  └─ ICMPForwarder│                              │  │
│  │  └──────────────────┘                              │  │
│  └──────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────┘
```

### Server Packet Paths

**Path 1: Client request → K8s service (most common)**
```
Client TCP conn
  → readFromTCPConnWriteToEndpoint
    → UDPConnOverTCP.Read (parse 2-byte datagram header)
    → Parse IP header, RouteMapTCP.AddRoute(srcIP, conn)
    → buf[2]==1 → inject to gvisor stack
  → gvisor TCPForwarder/UDPForwarder
    → net.Dial to real k8s service IP:port
    → bidirectional io.Copy
```

**Path 2: K8s service response → Client**
```
Real k8s service responds
  → gvisor stack processes response
  → readFromEndpointWriteToTCPConn
    → copy gvisor section views straight into a pooled buffer (pkt.AsSlices, one copy)
      at the canonical IP offset, type prefix = 0, length header in place
    → conn.Write(frame) → TCP conn → Client
```

**Path 3: Inter-client routing (Client A → Client B)**
```
Client A sends packet to Client B's tun IP
  → readFromTCPConnWriteToEndpoint on A's conn
    → RouteMapTCP.Load(dst) → found B's ConnList
    → stamp length header in place, WriteToRoutePacket(dst, pkt)
      → B's bufferedTCP.writePacket takes a reference (no copy)
      → B's bufferedTCP.run → B's raw TCP conn → Client B → release()
```

**Path 4: Server TUN → Client (kernel-originated traffic)**
```
Server kernel sends IP packet to tun device
  → pumpTun reads into data[3:], routeOutbound sets type=1 → tunInbound
  → serverTransport.routeTun
    → RouteMapTCP.Load(dst)
    → found: stamp length header in place (no shift), WriteToRoutePacket(dst, pkt)
      → bufferedTCP takes a reference → Client TCP conn
    → not found: drop packet, log warning
```

---

## Client Data Flow

**Entry point:** `pkg/handler/connect.go:386` (`startLocalTunServer`)

**Listener:** `tun://` with forwarder to server's `gtcp://:10801`

### Architecture Diagram

```
                            Local Machine (Client)
┌──────────────────────────────────────────────────────────────┐
│                                                              │
│  ┌──────────┐     ┌──────────────────────────────────────┐   │
│  │ Local App│     │         ClientDevice                  │  │
│  │  (curl,  │     │                                      │   │
│  │  browser)│     │  readFromTun ──▶ parse IP             │  │
│  └────┬─────┘     │    │                                  │  │
│       │           │    ├─ src==dst ──▶ gvisor stack #1    │   │
│       ▼           │    │               (self-to-self)     │  │
│  ┌─────────┐      │    └─ else ──────▶ tunInbound         │  │
│  │   TUN   │◀────▶│                     │                 │  │
│  │ Device  │      │  writeToTun ◀──── tunOutbound         │  │
│  │         │      │                     ▲                 │  │
│  └─────────┘      └─────────────────────┼─────────────────┘  │
│                                         │                    │
│  ┌──────────────────────────────────────┼──────────────────┐ │
│  │            handlePacket              │                  │ │
│  │                                      │                  │ │
│  │  Forwarder.DialContext ──▶ TCP conn to server           │ │
│  │                              │                          │ │
│  │  writeToConn ◀── slot.inbound ◀── runConnPool ◀─ tunInbound│
│  │    └─ stamp length in place → raw TCP → server          │ │
│  │                                                         │ │
│  │  readFromConn ──▶ UDPConnOverTCP.Read into buf[2:]       │ │
│  │    ├─ type==0 ──▶ tunOutbound ──▶ writeToTun            │ │
│  │    └─ type==1 ──▶ gvisor stack #2                       │ │
│  │                     (inter-client)                      │ │
│  │                     └─ LocalTCPForwarder                │ │
│  │                        dials 127.0.0.1:port             │ │
│  │                        response → tunInbound → server   │ │
│  └─────────────────────────────────────────────────────────┘ │
│                                                              │
│  ┌──────────────────────────────────────────────────────────┐│
│  │ heartbeats: periodic ICMP keepalive → tunInbound → server││
│  └──────────────────────────────────────────────────────────┘│
└──────────────────────────────────────────────────────────────┘
```

### Client Packet Paths

**Path 1: Local app → K8s service (most common)**
```
App sends TCP/UDP to k8s service IP (e.g. 10.96.0.1:80)
  → kernel routes to TUN device (via route table)
  → ClientDevice.readFromTun (pumpTun)
    → read at buf[3:] (tunReserve), routeOutbound sets type at buf[2]=1
    → parse IP: src=198.18.0.100, dst=10.96.0.1
    → src != dst → tunInbound → runConnPool hashes dst to a slot → slot.inbound
  → writeToConn
    → stamp 2-byte length header in place at buf[0:2]
    → raw TCP conn → Server
  → Server processes (Path 1 above) → response comes back
  → readFromConn
    → UDPConnOverTCP.Read into buf[2:] (type at buf[2], IP at buf[3:])
    → buf[2]==0 → tunOutbound channel
  → writeToTun
    → write IP packet at data[3:] to TUN (skipping length header + type prefix)
    → kernel delivers response to App
```

**Path 2: Self-to-self traffic (ping own tun IP)**
```
App pings 198.18.0.100 (own tun IP)
  → TUN device → readFromTun
    → src==dst → gvisorInbound
    → gvisor stack #1 (NewLocalStack)
      → LocalTCPForwarder dials 127.0.0.1:port
      → response → tunOutbound → TUN → App
```

**Path 3: Inter-client traffic (Client B connects to local service)**
```
Client B sends packet to this client's tun IP
  → Server routes via RouteMapTCP → this client's TCP conn
  → readFromConn
    → buf[2]==1 → gvisorInbound
    → gvisor stack #2 (NewLocalStack)
      → LocalTCPForwarder dials 127.0.0.1:port
      → connects to local service
      → response → tunInbound → writeToConn → server
      → server routes back to Client B
```

**Path 4: Heartbeat keepalive**
```
Every KeepAliveTime:
  → generate ICMP echo to server's router IP
  → type at buf[2]=1 → tunInbound → broadcast to all conn-pool slots → server
  → server gvisor handles ICMP → response via Path 2
```

---

## Key Components

### RouteHub (shared routing state)

```go
type RouteHub struct {
    RouteMapTCP   *sync.Map      // srcIP → net.Conn (maps client IPs to their TCP connections)
    TCPPacketChan chan *Packet    // bridges unroutable packets from gvisor → TUN device
}
```

- `RouteMapTCP` enables the server to route packets between clients
- `TCPPacketChan` handles packets that gvisor forwards to the server's TUN device

### BufferedTCP (write buffering)

Wraps TCP connections stored in `RouteMapTCP`. When one client's read goroutine routes a packet to another client, the write goes through `BufferedTCP` to prevent blocking the reader. Without this, a slow client would stall packet processing for all clients.

The routing hot paths (`routeTun`, inter-client) hand the framed `*Packet` to `BufferedTCP.writePacket`, which takes one reference and enqueues it; `run()` writes the frame and releases the reference. No per-packet copy. The generic `Write([]byte)` (net.Conn) still copies — the contract lets the caller reuse its buffer — but it is off the routing hot path.

### Two Client Gvisor Stacks

| Stack | Created in | Purpose | Input | Output |
|-------|-----------|---------|-------|--------|
| #1 | `clientTransport.routines` (`client-gvisor`) | Self-to-self traffic | `t.gvisorInbound` channel (src==dst) | `tunOutbound` → TUN |
| #2 | `runConnPool` (`interClientStack`) | Inter-client traffic | direct `InjectIP` from every slot's `readFromConn` (type==1) | `tunInbound` → runConnPool → slot → server |

Both use `NewLocalStack` with `LocalTCPForwarder` (dials `127.0.0.1`) and `LocalUDPForwarder`.

**Both stacks are transport-level, not per-slot.** For inter-client stack #2 each slot's
`readFromConn` injects `packetTypeToGvisor` packets **directly** into the shared stack via
`interClientStack.InjectIP` — mirroring the server's `readFromTCPConnWriteToEndpoint`. There is no
intermediate Go channel: an earlier design used a bounded `interClientInbound` channel with
drop-if-full, which under a bulk A→B transfer became silent receive-side TCP segment loss and
collapsed cross-client throughput (it got *worse* with concurrency). Direct injection makes flow
control TCP's own receive window instead of a blind drop, and — because `InjectInbound` returns
promptly (the localhost copy runs in the forwarder's goroutines) — never stalls a slot's reader, so
heartbeat-reply liveness on the data conns is preserved. See
[50-crossclient-throughput-diagnosis.md](../../docs/50-crossclient-throughput-diagnosis.md).

Stack #2 feeds its output to the shared `tunInbound`, which `runConnPool` dispatches to a slot by
five-tuple hash. So reconnecting or tearing down any one pool slot never destroys an in-flight
inter-client transfer (this used to be a per-slot stack bound to the slot's `subCtx` — see the
bug-fix note below).

---

## Performance Characteristics

### Per-packet overhead (client → k8s service → client roundtrip)

| Step | Operation | Cost |
|------|-----------|------|
| TUN read | syscall | ~1μs |
| Parse IP header | CPU | ~100ns |
| Channel send/recv (x4) | goroutine scheduling | ~200ns each |
| writeToConn (frame in place, raw TCP) | 0 extra copy | ~50ns |
| TCP write | syscall + possible TLS | ~5-50μs |
| Server gvisor inject | packet processing | ~1-5μs |
| TCPForwarder dial (first packet) | TCP handshake | ~1-100ms |
| io.Copy (bidirectional) | per-packet memcpy | ~200ns |
| Response: same path in reverse | | |

### Optimizations applied

All built on the single canonical layout (`[2 len][1 type][IP]`, IP reserved at `data[3:]`)
and reference-counted packet buffers.

**1. Unified layout (no shifts):** every producer/consumer agrees on the offsets, so the
loopback path no longer shifts `[type][IP]` between offsets, and `writeToTun` /
`readFromGvisorInbound` read the IP straight from `data[3:]`. The two former layout-shift
copies are gone.

**2. In-place datagram framing (`writeDatagram`):** the length header is stamped into the
reserved 2-byte head — no framing scratch buffer. Short writes continue on the same stream;
idempotent header stamping does not authorize replay after partial transmission.

**3. Zero-copy routing into BufferedTCP:** `routeTun` and inter-client routing hand the
`*Packet` to `bufferedTCP.writePacket` by reference (one reference transferred, released
after the socket write) instead of copying through `Write([]byte)`.

**4. Heartbeat fan-out:** shares the refcount where safe; the broadcast itself clones per
slot (each slot's `writeToConn` stamps the header in place and writes concurrently, so a
shared buffer would be a data race). Heartbeats are infrequent, so the clone is negligible.

**5. gvisor boundary copy halved:** `copyPacketToPool` and `readFromEndpointWriteToTCPConn`
copy gvisor's section views (`pkt.AsSlices()`, aliased — no copy) straight into the pooled
buffer in one pass, instead of `pkt.ToView()` (which flattens into a throwaway buffer first)
followed by a second copy.

**6. Batch activity accounting:** the slot writer sums packet counts and payload bytes for
`frameBatchWriter`'s complete prefix, then commits one `slotActivity` update (one clock read
and one slot lock), even when that prefix accompanies an error or the full batch completed
with an error. A partial frame and the unwritten suffix do not count. Bytes include the
one-byte type and exclude the two-byte length header. This reduces accounting overhead,
not health checks: successful writes do not establish heartbeat health.

**7. Single-member route snapshots:** `routeMembersSnapshot` is a value holding one
`*routeMember` directly in the common one-member case, avoiding the copied slice allocation.
Multiple members still use a copied slice. This changes allocation only: member identity,
stale-snapshot validation, newest-first fallback, write serialization and identity-checked
removal remain unchanged. Removing/re-adding the same conn creates a fresh membership that
an old write cannot evict.

**8. Retained TUN buffers and qualified direct reads:** unused pump buffers stay owned across
short batches; Linux native reads can fill pooled buffers without the adapter copy. See the
read section above for error/cancellation ownership and the capacity/memory trade-off.

### Remaining copies (unavoidable)

1. **`buffer.MakeWithData` in gvisor InjectInbound** — gvisor copies data into its own managed buffer. Cannot be eliminated without modifying gvisor internals.

2. **gvisor → pool boundary (`copyPacketToPool`, `readFromEndpointWriteToTCPConn`)** — one copy out of gvisor's reference-counted, chunked memory into our flat pooled buffer for wire framing. Reference counting addresses sharing, not this representation change; already minimized to a single copy.

3. **`bufferedTCP.Write([]byte)`** — the net.Conn contract lets the caller reuse the buffer, so this generic path copies. The routing hot paths avoid it via `writePacket`.

### Remaining optimization opportunities

1. **Lazy client gvisor stack #2**: Inter-client traffic is uncommon. Deferring stack creation until the first `type==1` packet from the server would save ~2MB memory.

### Bug fixes: heartbeat and reconnection

**ICMP echo reply (connection stability):**
The `ICMPForwarder` previously just logged and dropped ICMP packets. Since the gvisor stack has no assigned addresses, echo replies were never generated, causing client connections to timeout every ~60s. Fixed by implementing echo reply generation via `FindRoute` + `WritePacket`. Client read deadline also increased from 1x to 3x `KeepAliveTime`.

**Proactive route registration on reconnection (route recovery speed):**
After reconnection, the server could not register the client's route until it received the first data packet (which triggers `AddRoute`). The heartbeat ticker fired on a fixed 60s schedule unaware of reconnection events, causing 0-58s delay before route recovery. Fixed by having each slot proactively announce its TUN IP **directly on the freshly dialed conn** before the read/write loops start (`connSlot.run` → `clientTransport.registrationPayloads`): the server registers the route at once, with no dependency on the periodic heartbeat or the (droppable) broadcast path. Also moved the 2s backoff sleep to only fire on dial failure.

Result: route recovery after reconnect dropped from 0-58s to < 100ms.

(An earlier fix used a `reconnected` channel that signalled the heartbeat goroutine to fire an immediate broadcast heartbeat — see REFACTOR.md Iteration 14. It was replaced by the direct per-conn registration above, which is drop-immune and targets the exact new conn.)

**Inter-client transfer aborted when a pool slot reconnected (mid-transfer disconnect):**
The inter-client gvisor stack used to be **per-slot**, created in `readFromConn` bound to the
slot's `subCtx`, with its output written through that same slot's `writeToConn`. Reverse traffic
(the flow's ACKs) is routed to whichever slot last registered the client's IP on the server
(`RouteMapTCP` is last-writer-wins, and the heartbeat broadcasts on every slot make it flap), so a
slot could be busy sending a bulk transfer yet receive nothing on its own conn, hit `readFromConn`'s
180s read-idle timeout, be torn down, and destroy the gvisor stack it hosted — aborting the transfer
(`read i/o timeout` → forwarder `operation aborted` → RST → RTO retransmit storm). Two-part fix:
1. **Liveness is active-in-either-direction:** `writeToConn` now pushes the read (liveness) deadline
   forward on every successful write, so an actively-sending slot is never falsely torn down; a slot
   idle in both directions still times out.
2. **Stack decoupled from slots:** the inter-client stack is now transport-level (see the Two Client
   Gvisor Stacks note above), so even a genuinely idle slot's teardown cannot destroy it.

See `docs/08-heartbeat-health.md` §#3.
