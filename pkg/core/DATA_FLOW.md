# KubeVPN Core Data Flow Analysis

## Overview

KubeVPN uses a TUN device + gvisor network stack to create a VPN tunnel between a local client and a remote K8s cluster. All traffic between client and server is multiplexed over a single TCP connection using a custom datagram protocol (2-byte length header + payload).

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
may deliver several packets (wireguard-go GRO offload) into a batch of these buffers.

### Type prefix

The 1-byte type prefix at `data[2]` (named constants in `packet.go`); it is a small
extensible discriminator — values `2..255` are reserved for future packet types:

| Prefix | Constant | Meaning | Action at receiver |
|--------|----------|---------|--------------------|
| `0` | `packetTypeToTUN` | Gvisor-processed packet (response from real network) | Write to TUN device directly |
| `1` | `packetTypeToGvisor` | Raw IP packet (needs gvisor processing) | Inject into gvisor stack |

## Batched / segment-aware TUN reads

The TUN device is a wireguard-go `tun.Device`, whose API reads/writes **multiple packets per
syscall**: `Read(bufs [][]byte, sizes []int, offset int)`. `tunDevice.pumpTun` picks the loop
based on the device:

- **`pumpTunBatch`** (real TUN devices) — grabs `BatchSize()` pooled buffers, issues one
  batched `ReadPackets`, then parses and dispatches **each** returned packet individually into
  the canonical `[2 len][1 type][IP]` layout. Unused buffers from a short batch go back to the
  pool. This is the segment-aware path.
- **`pumpTunSingle`** (plain `net.Conn`, e.g. `net.Pipe` in tests) — the original one-packet
  loop.

**Why "segment-aware" matters:** on Linux, wireguard-go negotiates GRO/GSO offload, so a
single read can return **GRO-coalesced** packets (up to ~64 KB) and up to 128 of them per
call. `pumpTunBatch` treats each as its own IP packet. The offset bridge, the Linux-only
nature of offload (macOS/Windows have `BatchSize()==1`, so this path degrades to one packet
per read), and the 64 KB frame-boundary guard (oversized coalesced packets are dropped; TCP
recovers) all live in the `pkg/tun` adapter — see `docs/22-tun-device.md` §4.

The per-packet copy out of wireguard's scratch into the pooled buffer is the **same single
copy** the pre-batch code already performed (offsets differ and GRO may reallocate wireguard's
buffers, so the buffer cannot be shared) — batching does not add copies, it amortizes syscalls.

## Datagram Protocol (UDP-over-TCP)

TCP is a stream protocol, so packets are framed with a 2-byte big-endian length header:

```
[2 bytes: payload length][N bytes: payload (type prefix + IP packet)]
```

Read: `readDatagramPacket` reads the 2-byte length, then reads exactly N bytes.
Write: `writeDatagram(w, buf, payloadLen)` stamps the length header in place in the
buffer's reserved head and writes the frame `buf[:2+payloadLen]` in a single Write. It
is idempotent (safe to retry across connections) and emits one contiguous Write, so it
composes with `bufferedTCP` (one Write == one frame).

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

| Stack | Created in | Purpose | Output |
|-------|-----------|---------|--------|
| #1 | `readFromTun` | Self-to-self traffic | `tunOutbound` → TUN |
| #2 | `readFromConn` | Inter-client traffic | `tunInbound` → server |

Both use `NewLocalStack` with `LocalTCPForwarder` (dials `127.0.0.1`) and `LocalUDPForwarder`.

Stack #2 is **per-slot**: it is created inside `connSlot.readFromConn` and bound to that slot's
`subCtx`, so it lives and dies with the slot. Its output is written back out through the *same*
slot's `writeToConn`. This means the slot that hosts an active inter-client transfer is busy
**sending** even while its reverse traffic (ACKs) may arrive on a different slot — see the
slot-liveness bug fix below and `docs/08-heartbeat-health.md` §#3.

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
reserved 2-byte head and the frame written in one contiguous Write — no scratch buffer.
Idempotent, so it stays correct across `WriteFunc` retries.

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

**Actively-sending slot kept alive (mid-transfer disconnect):**
A slot hosts the per-slot inter-client gvisor stack (#2 above) and writes its output via
`writeToConn`, but the reverse traffic (that flow's ACKs) is routed to whichever slot last
registered the client's IP on the server (`RouteMapTCP` is last-writer-wins, and the heartbeat
broadcasts on every slot make it flap). So a slot could be busy sending a bulk transfer yet
receive nothing, hit `readFromConn`'s 180s read-idle timeout, be torn down, and destroy the
gvisor stack it hosts — aborting the transfer (`read i/o timeout` → forwarder `operation aborted`
→ RST → RTO retransmit storm). Fixed by having `writeToConn` push the read (liveness) deadline
forward on every successful write, so liveness is *active-in-either-direction*; a slot idle in
both directions still times out. See `docs/08-heartbeat-health.md` §#3.
