# Gvisor Network Stack Design

## 1. Overview

The core package (`pkg/core`) implements the VPN tunnel network layer using Google's gvisor userspace TCP/IP stack. It handles all packet forwarding between TUN devices and remote connections, supporting TCP, UDP, and ICMP protocols. This is the largest package in the project (~8200 lines).

## 2. Architecture

```
                     Local Machine (Client)                          Traffic Manager (Server)
┌─────────────────────────────────────────────┐     ┌──────────────────────────────────────────┐
│                                             │     │                                          │
│  App ──► TUN Device ──► ClientDevice        │     │      Device ◄── TUN Device               │
│               ▲            │                │     │        │           ▲                      │
│               │            │ direct slot 0 │     │        │           │                      │
│               │            ▼                │     │        ▼           │                      │
│          tunOutbound    slot[0]              │     │     RouteHub ──► tunOutbound              │
│               ▲            │                │     │        ▲                                  │
│               │            ▼                │     │        │                                  │
│         readFromConn   writeToConn          │     │   readFromRemote                          │
│               │            │                │     │        │                                  │
│          UDPOverTCP   UDPOverTCP            │     │   UDPOverTCP                              │
│               │            │                │     │        │                                  │
│               └──── TCP ───┘                │     │        │                                  │
│                     │                       │     │        │                                  │
└─────────────────────│───────────────────────┘     └────────│──────────────────────────────────┘
                      └─────── TLS (optional) ──────────────►┘
                              port 10801

  Sidecar (in-cluster)
┌─────────────────────────────────────────────┐
│  gvisor Stack (NewStack / NewLocalStack)     │
│  ├── TCPForwarder → dial real dst → relay    │
│  ├── UDPForwarder → dial real dst → relay    │
│  └── ICMPForwarder → raw socket forward      │
│       ▲                                      │
│       │ IP packets                           │
│   TUN Endpoint ◄── TUN Device                │
└─────────────────────────────────────────────┘
```

## 3. Protocol Registry

Protocols are registered via `RegisterProtocol(name, factory)` at `init()` time:

| Protocol | Factory | Listener | Handler | Purpose |
|----------|---------|----------|---------|---------|
| `tun` | `tunProtocolFactory` | TUN device | `tunHandler` | VPN tunnel (client/server) |
| `gtcp` | `gtcpProtocolFactory` | TCP on gvisor | `gvisorTCPHandler` | Accepts TCP connections from VPN clients |
| `gudp` | `gudpProtocolFactory` | TCP (datagram-framed) | `gvisorUDPHandler` | Accepts UDP-over-TCP from VPN clients |
| `ssh` | `sshProtocolFactory` | TCP | SSH handler | Fargate mode SSH tunnel |

`GenerateServers(listeners, hub)` parses URI strings and creates servers using the registry.

## 4. Node URI Format

Listener/forwarder URIs follow: `protocol://[host:port][/forward-uri][?key=value&...]`

Examples:
- `gtcp://:10801` — listen for gvisor TCP connections
- `tun://?net=198.18.0.100/32` — create TUN with specified IP
- `tun:/tcp://10.0.0.1:10801?route=10.96.0.0/12` — TUN client forwarding to server

## 5. TUN Handler (Server vs Client)

`tunHandler.Handle()` creates a `tunDevice` and chooses its transport before `serve`:
- **No forwarder** → `serverTransport` — this is the traffic manager
- **Has forwarder** → `clientTransport` — this is the local machine or sidecar

### 5.1 Server Side (`serverTransport`)

```
TunHandler → tunDevice + serverTransport → serve:
  pumpTun → routeOutbound → tunInbound → routeTun → WriteToRoutePacket(ctx, ...) → remote conn
  writeToTun ← tunOutbound ← routeTCPToTun ← RouteHub.tunQueue ← enqueueTUN(ctx, packet)
```

Server queue sends and packet routing are context-aware. `stopTUNBridge` signals stop,
joins in-flight `enqueueTUN` admission, and drains the private queue without closing its
data channel. Device `serve` joins routines before the final device-queue drain.
Server-role `FatalOnReturn()` makes a returned TUN handler terminal for `handler.Run`:
cancel, close listeners/connections, join accept loops and accepted handlers. Ordinary
TCP EOF is not fatal; root-client NetworkManager completion is unchanged (see [12](12-session-lifecycle.md)).

The server reads IP packets from the TUN device, looks up the destination in `RouteHub`, and writes to the appropriate remote connection. Incoming packets from remote connections are written back to the TUN device.

**Packet framing**: IP data is read at `buf[3:]`, leaving room for `[2-byte length][1-byte type prefix]`. The type prefix byte (`1` = IP packet) distinguishes different payload types.

**Shared read loop**: both server and client read from the TUN via one generic loop,
`tunDevice.pumpTun(ctx, errLabel, dispatch)` (in `tun_device.go`). It owns the pooled-buffer
read at `buf[3:]`, `ParseIPFast`, and read/parse error handling (read error → `errChan` + stop;
parse error → drop + continue), then hands each packet to a per-side `dispatch` callback.
The server's `routeOutbound` dispatch debug-logs and sends to `tunInbound`, releasing the
packet if cancellation prevents admission.

### 5.2 Client Side (`clientTransport`)

```
tunDevice + clientTransport:
  pumpTun     ──► tunInbound ──► runConnPool ──► slot[0] (direct)     ──► writeToConn ──► server
                                                                                        │
  writeToTun  ◄── tunOutbound ◄──────────────── readFromConn ◄──────────────────────────┘
```

`clientTransport.routeOutbound` is dispatched by the same `pumpTun` loop; it sets the
type prefix and either forwards locally (when `src == dst`, via the gvisor handler) or pushes
to `tunInbound` for the pool to distribute.

**Connection pool**: The default is **one data slot plus one independent control slot**. Outbound data goes directly to slot 0 without parsing/hashing for slot selection. The private `poolSize` remains a data-only override for nil-stats tests/benchmarks comparing 1/2/4/8 slots; there is no CLI or environment flag. Multi-slot tests retain `flowHash` affinity, falling back to `ipHash(dst)` for packets without usable ports. The server shares one gvisor stack per client across its conns, with grace retention across reconnects independent of slot count (see [48-shared-server-gvisor-stack.md](48-shared-server-gvisor-stack.md)). One data conn multiplexes concurrent business flows; it does not limit business concurrency to one. See [41-five-tuple-pool-dispatch.md](41-five-tuple-pool-dispatch.md).

**`connSlot`**: each slot retains its inbound queue, connection lifecycle (`run` dial/reconnect
loop), `readFromConn`, `writeToConn`, deadlines and bounded batching. `runConnPool` starts
`go slot.run(ctx)` and uses blocking data-queue sends to preserve backpressure. Default dispatch
selects slot 0 directly; only multi-slot tests hash for selection. Both reader and writer refresh
conn deadlines lazily via `periodicDeadline` rather than resetting them per packet.

**Heartbeats**: Periodic ICMP requests use the independent control queue/conn to the **same
TCP forwarding endpoint 10801**, not xDS 9002, bypassing the business queue. Control is excluded
from RouteHub; replies use the registered data conn. The outer SSH/port-forward TCP transport
is still shared and replies depend on data-path progress, so this is not an absolute liveness
guarantee.

**Reconnection**: Each slot reconnects independently on failure. After reconnecting, the slot proactively announces its TUN IP directly on the new conn (`registrationPayloads`), so the server re-registers the route immediately — no waiting for the next periodic heartbeat.

### 5.3 File layout

| File | Responsibility |
|------|----------------|
| `packet.go` | The `Packet` value type (`NewPacket`/`Data`/`Length`), the gvisor `copyPacketToPool` helper, and `logIPPacket` (the debug packet-log helper — the only `gopacket/layers` user among these files) |
| `tun_device.go` | Shared `tunDevice` base (`tun`, the inbound/outbound channels, `errChan`), `writeToTun`, `Close`, the generic `pumpTun` read loop, and `drainPacketChan` |
| `tun_client.go` | `clientTransport`, connection pool dispatch and heartbeats |
| `conn_slot.go` | `connSlot` queues, dial/reconnect and read/write loops |
| `tun_server.go` | `serverTransport` routing (`routeTun`/`routeTCPToTun`) |
| `tun_handler.go` | Role selection, `FatalOnReturn`, and joined device `serve` |

## 6. RouteHub — Packet Routing

`RouteHub` is the central routing table shared between TUN and gvisor handlers:

```go
// Selected fields; private metadata is accessed through RouteHub operations.
type RouteHub struct {
    mu          sync.Mutex
    routes      map[string]*ConnList
    tunQueue    chan *Packet
    tunDone     chan struct{}
    emptyLocked func(ipKey string)
    addedLocked func(ipKey string)
}
```

**ConnList**: Holds newest-first data-route member identities; control conns are excluded.
The default has one data conn per client address. Writes snapshot membership, validate the
current list/member before I/O, and remove failures only if those identities still match.
A stale write therefore cannot delete a replacement list or a re-added member. Neither
`hub.mu` nor `ConnList.mu` spans socket writes, enqueue, or Close. Raw frame serialization
uses a separate gate; buffered writers own their queue serialization.

**Cancellation and fallback**: `WriteToRoutePacket(ctx, dstKey, pkt)` cancels buffered queue
admission without evicting a healthy conn. Cancellation while waiting for the raw gate or
before I/O leaves the conn intact; cancellation during active raw socket I/O closes the
selected conn and waits for the close callback. Fallback is only safe after zero wire
progress, never after a partially transmitted frame or cancellation. `writeFull` continues
short writes on the same conn. This is first-writable routing, not per-flow balancing.
Legacy `WriteFuncToRoute` callbacks lack byte-progress/cancellable-I/O guarantees and are
not used for the framed-packet path.

**Route registration/lifetime**: `AddRoute(ctx, srcIP, conn)` registers data packets and
proactive registration echoes, but refuses a closed `bufferedTCP`. Its stop callback removes
routes even before reader cleanup. Route changes and internal `addedLocked`/`emptyLocked`
callbacks are serialized under `hub.mu`; callback lock order is hub then `handler.mu`,
with metadata/timer updates only (no I/O, logging, or hub reentry). Periodic control probes
do not register routes. See [48](48-shared-server-gvisor-stack.md) for grace epochs and
[08](08-heartbeat-health.md) for read/write deadlines.

## 7. Protocol Encapsulation

### 7.1 UDPOverTCP (`conn_udp_over_tcp.go`)

Provides UDP-like datagram semantics over a TCP stream using length-prefix framing:

```
[2-byte big-endian length][payload]
[2-byte big-endian length][payload]
...
```

Used by the TUN client→server connection to preserve packet boundaries. The uint16 length
allows **0..65535 payload bytes**, including any tunnel type prefix. The framing layer can
represent an empty datagram; higher-level consumers may reject or ignore it.

`readDatagramPacket` reads its two-byte header independently of the caller's payload
storage. Insufficient storage returns `io.ErrShortBuffer`; because the header was consumed,
the stream is unusable and the wrapper closes it. Oversize outbound payloads are rejected
before writing. Specifically, `UDPConnOverTCP.Write` uses standalone storage for its
maximum-size frame (**65537 bytes**), without returning that storage to `config.LPool`.
This allocation contract belongs to the wrapper, not every framed-packet path. The uint16
wire limit is unchanged; other pooled paths can reject packets when their buffer is too short.

`UDPConnOverTCP` uses separate read/write mutexes to preserve framing while allowing
full-duplex I/O. Its context cancellation and Close close the transport without taking
those I/O mutexes. Framing read/write errors terminate the stream, not a resumable frame.

### 7.2 Shared framing and UDP relay

`writeDatagram` stamps reserved headroom and uses the common `writeFull` helper. `writeFull`
reports actual wire progress and errors, continues positive short writes only on the same
writer, and converts zero progress without an error to `io.ErrShortWrite`. A partial frame
failure must never trigger fallback/replay onto another connection.

`relayUDPOverTCP` in `gvisor_udp_handler.go` closes both TCP and UDP endpoints on first copy
completion or cancellation, then joins both copy workers. This is the UDP-over-TCP relay's
ownership contract, not a claim about all gvisor forwarder goroutines.

### 7.3 BufferedTCP (`conn_buffered_tcp.go`)

Wraps a TCP connection with an **async write queue** to coalesce syscalls under load.
`NewBufferedTCP` starts a single `run(ctx)` goroutine that drains a buffered channel
(`ch chan *Packet`, capacity `MaxSize`) and writes each framed packet to the socket. Two enqueue
paths:

- **`writePacket(ctx, pkt)`** — the hot path. Enqueues an *already-framed* `Packet` and
  **transfers one reference on success**; the writer releases it after writing or draining.
  On cancellation/closed error, ownership is not taken. Admission cancellation alone does
  not evict a healthy route.
- **`Write(b)`** — the `net.Conn` contract path copies bytes because the caller may reuse
  them immediately. Its existing larger-buffer allocation and `Packet.release` pool behavior
  are unchanged by this increment; the standalone-storage guarantee above applies only to
  `UDPConnOverTCP.Write`.

Stop sets `closed`, closes the admission-stop `done` channel, and invokes the installed
route-removal callback. Close then closes the underlying transport outside route metadata
locks. `run` also closes on exit, including idle/pre-canceled cases, joins in-flight enqueue
admission and drains the remaining packets. The data channel is never closed; `done` is
not a writer-completion join signal. Closed writers cannot be registered again.

### 7.4 Reference-Counted Packet Pool (`packet.go`)

The data plane avoids per-packet allocation and copying via a single canonical buffer layout and a
reference count. Every `Packet` wraps a pooled `[]byte` (`config.LPool`) with reserved headroom so
the wire frame can be stamped in place:

```
data[0:2] = datagram length header (datagramHeaderLen)
data[2]   = type prefix (typePrefixLen): 0=write to TUN, 1=inject into gvisor
data[3:]  = raw IP payload (starts at tunReserve = 3)
wire frame = data[0 : datagramHeaderLen+length]
```

Reference counting (`refs atomic.Int32`) lets one buffer be shared by N consumers **without
copying** — e.g. explicit multi-slot test fan-out (`broadcastToSlots`) or an async
`bufferedTCP` queue:

- `refs` counts *extra* references beyond the implicit owner, so the zero value (a plain
  `&Packet{}`) is a valid single-owner packet freed by exactly one `release()`.
- `acquire()` adds a reference; `release()` drops one and returns the buffer to `config.LPool` when
  the implicit owner's ref is dropped (`refs` reaches `-1`).
- Releasing more times than acquired **panics** (`released more times than acquired`) — a
  use-after-free guard rather than silent corruption.

This is modeled on gvisor's chunk refcount (`buffer.View.Clone`/`Release`). Type-prefix values
`2` identifies control/heartbeat frames; `3..255` remain reserved.

## 8. Gvisor Stack

### 8.1 Stack Creation

`newGvisorStack(ctx, tun, tcpFwd, udpFwd)` creates a gvisor network stack with:
- IPv4 + IPv6 network protocols
- TCP + UDP + **ICMP** transport protocols (`tcp`, `udp`, `icmp.NewProtocol4`, `icmp.NewProtocol6`). ICMP **must** be registered here: gvisor's `SetTransportProtocolHandler` is a no-op for a protocol that is not in this list, so without it the `ICMPForwarder` handler below is never installed and echo requests (ping, and the heartbeat echo replies in `08-heartbeat-health.md`) get no response.
- SACK, TTL=64, moderate receive buffer, forwarding enabled
- Stock gvisor TCP send/receive buffer ranges (autotuning up to 4MB via `ModerateReceiveBuffer`). NOTE: raising these to Tailscale's 8MiB/6MiB was tried and reverted — Tailscale assumes one UDP flow per peer, but KubeVPN multiplexes every flow over a shared TCP tunnel, where an oversized single-flow buffer causes head-of-line bloat that starves other flows (see `08-heartbeat-health.md` §#3)
- `GVisorGSOSupported` on the link endpoint of **both** the server stack (`gvisor_tcp_handler.go`) and the client local stacks (`gvisor_local_tcp_handler.go`, self-to-self + inter-client) — gvisor performs software segmentation internally, complementing the TUN-side GRO (`docs/22-tun-device.md` §4.2). We deliberately avoid `HostGSOSupported`: these endpoints feed a TCP tunnel, not a host NIC, so host-GSO would push super-MTU segments onto the wire that macOS/Windows clients cannot write to their TUN. (TX checksum offload is deliberately NOT enabled: gvisor-generated inner packets are ultimately delivered to a real kernel that validates the checksum, so skipping it would cause silent drops — unlike RX checksum offload, which only skips gvisor's own injection-time validation.)
- Promiscuous mode + spoofing enabled (accepts all packets)
- Default routes for all IPv4 and IPv6 traffic

Two variants:
- `NewStack()` — TCP/UDP forwarded to real destinations (production sidecar)
- `NewLocalStack()` — forwarded to localhost (dev/test mode)

### 8.2 Transport Forwarders

**TCPForwarder** (`gvisor_tcp_forwarder.go`):
- Intercepts TCP SYN from gvisor stack
- Created with rcvWnd = `tcp.DefaultReceiveBufferSize` (not 0, which would start at the minimum window) and `tcpForwarderMaxInFlight` = 8192 (Tailscale's Linux value)
- Dials the real destination (e.g., the actual service IP)
- Relays data bidirectionally between gvisor gonet.Conn and the real TCP conn
- Both relay goroutines are guaranteed to finish: after the first completes, both sides get `SetReadDeadline(1s)` to unblock the second goroutine

**UDPForwarder** (`gvisor_udp_forwarder.go`):
- Intercepts UDP packets from gvisor stack
- Dials the real destination UDP address
- Relays with configurable timeouts (5-minute idle)

**ICMPForwarder** (`gvisor_icmp_forwarder.go`):
- Forwards ICMP echo requests via raw socket
- Returns responses back through the gvisor stack

### 8.3 TUN Endpoints

**gvisor_tun_endpoint.go**: Server-side, bridges tunnel conns to a **per-client** gvisor
`channel.Endpoint`. `readFromTCPConnWriteToEndpoint` routes registered peer traffic directly,
injects cluster-bound traffic into that client's stack, or hands packets to `enqueueTUN`.
`readFromEndpointWriteToRoute` uses blocking, context-aware `WriteToRoutePacket`; each client
has an independent output loop, rather than a single shared loop with non-blocking drops.
Oversized stack output that cannot fit its pooled framed buffer is dropped, not truncated.
The wire framing layer's maximum remains 65535 payload bytes.

Stack grace expiry checks the expected `clientStack` identity, armed epoch, and absence of
a current data route under hub then handler locks. Control-before-data creation gets one
bounded grace interval, not one refreshed by probes; data registration invalidates it.
`clientStack.done` covers the owned output loop plus stack destruction only, not every
foreign-library goroutine. See [48-shared-server-gvisor-stack.md](48-shared-server-gvisor-stack.md).

**gvisor_local_tun_endpoint.go**: Client-side, bridges between the local TUN device and gvisor for local traffic (src==dst).

## 9. Sidecar IP Allocation

The `tun` protocol factory handles sidecar self-DHCP:
1. If `net=` parameter is empty, call `requestTunIPFromControlPlane()` → gRPC `GetTunIP` to TunConfigServer
2. Create the TUN device with the returned IP

**Control-plane handshake** (`requestTunIPFromControlPlane`): the sidecar dials
`<TrafficManagerService>:<PortControlPlane>` (service address from the `TrafficManagerService` env)
with a 30s blocking dial, then calls `GetTunIP{OwnerID, Namespace, Hostname}`. `OwnerID` is the
pod name (`EnvPodName`, falling back to `"unknown"`); `Hostname` is the pod hostname recorded in
`TUN_ALLOCS` for debugging. There is **no explicit release**: per the DHCP protocol, when the
process exits its `WatchTunIP` stream disconnects, and after `LeaseDuration` without renewal the
server's lease reaper reclaims the IP (see [03-dhcp-ip-allocation.md](03-dhcp-ip-allocation.md)).

The factory fetches the IP **once at startup** and does not run any background watcher. The sidecar's own TUN IP is fixed for the process lifetime; runtime routing changes (when a *client's* IP changes) are handled by the control-plane (`syncEnvoyRuleIP`) and pushed to the sidecar's envoy via xDS — not by the sidecar editing its own device. The former `watchTunIPChanges` / `pollTunIP` / `applyTunIPChange` + `tun.UpdateDNAT` path was removed by the unified proxy mode (see [09-tun-ip-hot-update.md](09-tun-ip-hot-update.md) and [28-sleep-wake-ip-update.md](28-sleep-wake-ip-update.md)).

## 10. Forwarder

`Forwarder` dials remote addresses with retry, layering Transport and Connector:

```
Forwarder.DialContext()
  → Transporter.Dial(addr)     // TCP connection (with optional TLS)
  → Connector.ConnectContext() // wrap with UDPOverTCP framing
  → returns net.Conn
```

## 11. Related Files

| File | Purpose |
|------|---------|
| `gvisor_stack.go` | gvisor stack creation (NewStack, NewLocalStack) |
| `tun_server.go` | Server transport and routing |
| `tun_client.go` | Client transport, connection pool, heartbeats |
| `tun_device.go` | Shared tunDevice and device I/O |
| `route.go` | RouteHub, ConnList, protocol registry, GenerateServers |
| `node.go` | Node URI parsing |
| `forwarder.go` | Forwarder, Handler, Connector, Transporter interfaces |
| `protocol_registry.go` | Protocol factory registration + tun/gtcp/gudp/ssh factories |
| `gvisor_tcp_forwarder.go` | TCP forwarding through gvisor |
| `gvisor_udp_forwarder.go` | UDP forwarding through gvisor |
| `gvisor_icmp_forwarder.go` | ICMP forwarding |
| `gvisor_tun_endpoint.go` | TCP↔gvisor bridge (server side) |
| `gvisor_local_tun_endpoint.go` | TUN↔gvisor bridge (client side) |
| `gvisor_tcp_handler.go` | gvisor TCP listener handler (gtcp) |
| `gvisor_udp_handler.go` | gvisor UDP listener handler (gudp) |
| `conn_udp_over_tcp.go` | UDP-over-TCP datagram framing |
| `gvisor_udp_handler.go` | UDP-over-TCP relay, endpoint close and copy-worker join |
| `conn_buffered_tcp.go` | Buffered TCP wrapper |
| `transporter_tcp.go` | TCP Transporter with optional TLS |
| `ssh.go` | SSH protocol listener/handler |
