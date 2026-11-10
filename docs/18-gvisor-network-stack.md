# Gvisor Network Stack Design

## 1. Overview

The tunnel package (`pkg/tunnel`) owns packet forwarding between TUN devices and remote connections. Its `internal/netstack` package constructs gvisor TCP/UDP/ICMP stacks and forwarders without importing the parent tunnel package. Shared length-prefix connections and batch socket I/O live in `pkg/internal/wire`; pooled `packet` references and packet batches remain in tunnel.

`pkg/endpoint` retains URI parsing, protocol factories, TCP/TLS dialing, SSH, UDP relay/reverse bridge and `Serve`. `endpoint.NewRouteHub` returns the real `*tunnel.Router` (not an alias). `Forwarder.TunnelConfig` supplies a dial function, preserving endpoint's retry/connector/TLS policy without a reverse dependency. `Serve` starts factory-created service handlers before accept and closes/joins their peers after connection handlers finish. `pkg/network` consumes tunnel handlers and heartbeat statistics.

## 2. Architecture

Diagram labels retain the older descriptive names: `ClientDevice` is now
`tunDevice` + `clientTransport`, and `readFromConn`/`writeToConn` are the attempt's
`readGeneration`/`writeGeneration`. `Packet` in the ownership discussion means the
now-private `packet` type in `pkg/tunnel/packet.go`, not an exported endpoint API.

```
                     Local Machine (Client)                          Traffic Manager (Server)
┌─────────────────────────────────────────────┐     ┌──────────────────────────────────────────┐
│                                             │     │                                          │
│  App ──► TUN Device ──► ClientDevice        │     │      Device ◄── TUN Device               │
│               ▲            │                │     │        │           ▲                      │
│               │            │ direct slot 0 │     │        │           │                      │
│               │            ▼                │     │        ▼           │                      │
│          tunOutbound    slot[0]              │     │     Router ──► tunOutbound              │
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
  writeToTun ← tunOutbound ← routeTCPToTun ← Router.bridge.queue ← bridge.send(ctx, packet)
```

Server queue sends and packet routing are context-aware. The separate `tunBridge`
(`pkg/tunnel/bridge.go`) owns `send` admission and `stop`/join/drain without closing its
data channel. `deviceSession` joins routines before the final transport/device-queue drain.
Server-role `FatalOnReturn()` makes a returned TUN handler terminal for `endpoint.Serve`:
cancel, close listeners/connections, join accept loops and accepted handlers, then close/join
service peers. Ordinary TCP EOF is not fatal; root-client completion is owned by
`network.Runtime` (see [12](12-session-lifecycle.md)).

The server reads IP packets from the TUN device, looks up the destination in `Router`, and writes to the appropriate remote connection. Incoming packets from remote connections are written back to the TUN device.

**Packet framing**: IP data is read at `buf[3:]`, leaving room for `[2-byte length][1-byte type prefix]`. The type prefix byte (`1` = IP packet) distinguishes different payload types.

**Shared read loop**: both server and client read from the TUN via one generic loop,
`tunDevice.pumpTun(ctx, errLabel, dispatch)` (in `pkg/tunnel/device.go`). It owns the pooled-buffer
read at `buf[3:]`, `ParseIPFast`, and read/parse error handling (read error → `errChan` + stop;
parse error → drop + continue), then hands an owning `*packet` to a per-side `dispatch` callback.
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

**Connection pool**: The topology is **one data slot plus one independent control slot**. Outbound data goes directly to slot 0 without parsing/hashing for slot selection; there is no pool-size setting, flag, or override. The server shares one gvisor stack per client across its conns, with grace retention across reconnects independent of slot count (see [48-shared-server-gvisor-stack.md](48-shared-server-gvisor-stack.md)). One data conn multiplexes concurrent business flows; it does not limit business concurrency to one. The removed five-tuple multi-slot dispatch is recorded in [41-five-tuple-pool-dispatch.md](41-five-tuple-pool-dispatch.md).

**`connSlot`** (`pkg/tunnel/slot.go`) retains its inbound queue and retry loop.
Each `connAttempt` (`pkg/tunnel/attempt.go`) owns exactly one dial/handshake and the
`readGeneration`/`writeGeneration` pumps. It closes the connection and joins both pumps
before the next attempt may consume the shared queue. `runConnPool` uses blocking data-queue
sends for backpressure; default dispatch selects slot 0, and only multi-slot tests hash.
`periodicDeadline` refreshes deadlines lazily; the attempt writer retains bounded batching.

**Heartbeats**: Periodic ICMP requests use the independent control queue/conn to the **same
TCP forwarding endpoint 10801**, not xDS 9002, bypassing the business queue. Control is excluded
from Router; `handleControlConn` synthesizes replies inline on that same control conn. The outer
SSH/port-forward TCP transport is still shared, so this is not an absolute liveness guarantee.

**Reconnection**: Each slot reconnects independently on failure. After reconnecting, the slot proactively announces its TUN IP directly on the new conn (`registrationPayloads`), so the server re-registers the route immediately — no waiting for the next periodic heartbeat.

### 5.3 File layout

| File | Responsibility |
|------|----------------|
| `pkg/tunnel/packet.go` | Private `packet`/`newPacket`, reference ownership, `copyPacketToPool`, packet logging |
| `pkg/tunnel/device.go` | Shared `tunDevice`, `writeToTun`, `Close`, `pumpTun` and queue drain |
| `pkg/tunnel/client.go` | `clientTransport`, pool dispatch and heartbeats |
| `pkg/tunnel/slot.go`, `pkg/tunnel/attempt.go` | Queue/retry lifetime; single-connection ownership and joined pumps |
| `pkg/tunnel/server_device.go` | `serverTransport` routing (`routeTun`/`routeTCPToTun`) |
| `pkg/tunnel/device_handler.go` | Role selection, `FatalOnReturn`, and `deviceSession` joined shutdown |
| `pkg/tunnel/packet_io.go` | Shared framed socket reader and reusable packet owner |
| `pkg/tunnel/server_runtime.go`, `route_table.go`, `route_writer.go` | Server composition root, route metadata, and write/fallback ownership |

## 6. Router — Packet Routing

`Router` is the public facade over the runtime shared by TUN and gvisor handlers:

```go
type Router struct { *serverRuntime }

type serverRuntime struct {
    *routeTable               // route map + peer transitions
    writer *routeWriter       // snapshot/write/fallback/failure reconciliation
    peers  *peerRegistry
    bridge *tunBridge
}
```

`peerRegistry` depends directly on `routeTable` and `routeWriter`, not back through the
facade.

**ConnList**: Holds newest-first data-route member identities; control conns are excluded.
The default has one data conn per client address. Writes snapshot membership, validate the
current list/member before I/O, and remove failures only if those identities still match.
A stale write therefore cannot delete a replacement list or a re-added member. Neither
`routeTable.mu` nor `ConnList.mu` spans socket writes, enqueue, or Close. Raw frame serialization
uses a separate gate; buffered writers own their queue serialization.

**Cancellation and fallback**: `WriteToRoutePacket(ctx, dstKey, pkt)` cancels buffered queue
admission without evicting a healthy conn. Cancellation while waiting for the raw gate or
before I/O leaves the conn intact; cancellation during active raw socket I/O closes the
selected conn and waits for the close callback. Fallback is only safe after zero wire
progress, never after a partially transmitted frame or cancellation. `WriteFull` continues
short writes on the same conn. This is first-writable routing, not per-flow balancing.
Legacy `WriteFuncToRoute` callbacks lack byte-progress/cancellable-I/O guarantees and are
not used for the framed-packet path.

**Route registration/lifetime**: `AddRoute(ctx, srcIP, conn)` registers data packets and
proactive registration echoes, but refuses a closed `bufferedTCP`. Its stop callback removes
routes even before reader cleanup. Route changes call `peerRegistry.onRouteAddedLocked` /
`onRouteEmptyLocked` under `routeTable.mu`, then the registry mutex, with metadata/timer updates
only (no I/O, logging, or hub reentry). The registry is started with the service context by
`endpoint.Serve`, never by the first accepted connection. Periodic control probes
do not register routes. See [48](48-shared-server-gvisor-stack.md) for grace epochs and
[08](08-heartbeat-health.md) for read/write deadlines.

## 7. Protocol Encapsulation

### 7.1 Datagram framing (`pkg/internal/wire/conn.go`)

The former `wire.Conn` is now `wire.Conn` (imported as `framing` by the tunnel).
The shared helpers are `ReadDatagram`, `WriteDatagram` and `WriteFull`; the legacy names
in the protocol discussion below refer to those operations, not APIs exported by endpoint.

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
before writing. Specifically, `wire.Conn.Write` uses standalone storage for its
maximum-size frame (**65537 bytes**), without returning that storage to `config.LPool`.
This allocation contract belongs to the wrapper, not every framed-packet path. The uint16
wire limit is unchanged; other pooled paths can reject packets when their buffer is too short.

`wire.Conn` uses separate read/write mutexes to preserve framing while allowing
full-duplex I/O. Its context cancellation and Close close the transport without taking
those I/O mutexes. Framing read/write errors terminate the stream, not a resumable frame.

### 7.2 Shared framing and UDP relay

`WriteDatagram` stamps reserved headroom and uses the common `WriteFull` helper. `WriteFull`
reports actual wire progress and errors, continues positive short writes only on the same
writer, and converts zero progress without an error to `io.ErrShortWrite`. A partial frame
failure must never trigger fallback/replay onto another connection.

`relayUDPOverTCP` in `pkg/endpoint/udp_relay.go` closes both TCP and UDP endpoints on first copy
completion or cancellation, then joins both copy workers. This is the UDP-over-TCP relay's
ownership contract, not a claim about all gvisor forwarder goroutines.

### 7.3 BufferedTCP (`pkg/tunnel/buffered_conn.go`)

Wraps a TCP connection with an **async write queue** to coalesce syscalls under load.
`newBufferedTCP` starts a single `run(ctx)` goroutine that drains a buffered channel
(`ch chan *Packet`, capacity `MaxSize`) and writes each framed packet to the socket. Two enqueue
paths:

- **`writePacket(ctx, pkt)`** — the hot path. Enqueues a `packet` with reserved length
  headroom and **transfers one reference on success**; `wire.BatchWriter` stamps before I/O,
  and the writer releases the reference after writing or draining.
  On cancellation/closed error, ownership is not taken. Admission cancellation alone does
  not evict a healthy route.
- **`Write(b)`** — the `net.Conn` contract path copies bytes because the caller may reuse
  them immediately. Its existing larger-buffer allocation and `Packet.release` pool behavior
  are unchanged by this increment; the standalone-storage guarantee above applies only to
  `wire.Conn.Write`.

Stop sets `closed`, closes the admission-stop `done` channel, and invokes the installed
route-removal callback. Close then closes the underlying transport outside route metadata
locks. `run` also closes on exit, including idle/pre-canceled cases, joins in-flight enqueue
admission and drains the remaining packets. The data channel is never closed; `done` is
not a writer-completion join signal. The connection handler separately calls `waitRun()`
(`runDone`) after cancellation/Close, so its owner joins the writer before returning.
Closed writers cannot be registered again.

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
copying** — e.g. fan-out to multiple consumers or an async
`bufferedTCP` queue:

- `refs` counts *extra* references beyond the implicit owner, so the zero value (a plain
  `&Packet{}`) is a valid single-owner packet freed by exactly one `release()`.
- `acquire()` adds a reference; `release()` drops one and returns the buffer to `config.LPool` when
  the implicit owner's ref is dropped (`refs` reaches `-1`).
- Releasing more times than acquired **panics** (`released more times than acquired`) — a
  use-after-free guard rather than silent corruption.

This is modeled on gvisor's chunk refcount (`buffer.View.Clone`/`Release`). Type-prefix values
`2` identifies control/heartbeat frames; `3..255` remain reserved.

`packetReader` (`pkg/tunnel/packet_io.go`) is the only framed-socket-to-packet constructor for
client/server data and control loops. `framing.Conn.ReadView` supplies an exactly sized ingress
View; packet metadata comes independently from `packetPool`, so every dispatch receives an
independent owner. gVisor injection transfers the View without copying, while asynchronous
routing and TUN output call `materialize()` into the canonical pooled layout. Channel success
transfers the reference, failure releases it, and canonical pool Get/Put is centralized in
`packet.go`. Client and server keep separate consume-packet dispatch functions, so routing,
heartbeat correlation and gVisor policy do not leak into the shared reader.

## 8. Gvisor Stack

### 8.1 Stack Creation

`netstack.New(ctx, tun, tcpFwd, udpFwd)` (`pkg/tunnel/internal/netstack/stack.go`) creates a gvisor network stack with:
- IPv4 + IPv6 network protocols
- TCP + UDP + **ICMP** transport protocols (`tcp`, `udp`, `icmp.NewProtocol4`, `icmp.NewProtocol6`). ICMP **must** be registered here: gvisor's `SetTransportProtocolHandler` is a no-op for a protocol that is not in this list, so without it the `ICMPForwarder` handler below is never installed and echo requests (ping, and the heartbeat echo replies in `08-heartbeat-health.md`) get no response.
- SACK, TTL=64, moderate receive buffer, forwarding enabled
- Stock gvisor TCP send/receive buffer ranges (autotuning up to 4MB via `ModerateReceiveBuffer`). NOTE: raising these to Tailscale's 8MiB/6MiB was tried and reverted — Tailscale assumes one UDP flow per peer, but KubeVPN multiplexes every flow over a shared TCP tunnel, where an oversized single-flow buffer causes head-of-line bloat that starves other flows (see `08-heartbeat-health.md` §#3)
- `GVisorGSOSupported` on the link endpoint of **both** the server stack (`pkg/tunnel/peers.go`) and the client local stacks (`pkg/tunnel/client_stack.go`, self-to-self + inter-client) — gvisor performs software segmentation internally, complementing the TUN-side GRO (`docs/22-tun-device.md` §4.2). We deliberately avoid `HostGSOSupported`: these endpoints feed a TCP tunnel, not a host NIC, so host-GSO would push super-MTU segments onto the wire that macOS/Windows clients cannot write to their TUN. (TX checksum offload is deliberately NOT enabled: gvisor-generated inner packets are ultimately delivered to a real kernel that validates the checksum, so skipping it would cause silent drops — unlike RX checksum offload, which only skips gvisor's own injection-time validation.)
- Promiscuous mode + spoofing enabled (accepts all packets)
- Default routes for all IPv4 and IPv6 traffic

Two variants:
- `NewStack()` — TCP/UDP forwarded to real destinations (production sidecar)
- `NewLocalStack()` — forwarded to localhost (dev/test mode)

### 8.2 Transport Forwarders

**TCPForwarder** (`pkg/tunnel/internal/netstack/tcp.go`):
- Intercepts TCP SYN from gvisor stack
- Created with rcvWnd = `tcp.DefaultReceiveBufferSize` (not 0, which would start at the minimum window) and `tcpForwarderMaxInFlight` = 8192 (Tailscale's Linux value)
- Dials the real destination (e.g., the actual service IP)
- Relays data bidirectionally between gvisor gonet.Conn and the real TCP conn
- Both relay goroutines are guaranteed to finish: after the first completes, both sides get `SetReadDeadline(1s)` to unblock the second goroutine

**UDPForwarder** (`pkg/tunnel/internal/netstack/udp.go`):
- Intercepts UDP packets from gvisor stack
- Dials the real destination UDP address
- Relays with configurable timeouts (5-minute idle)

**ICMPForwarder** (`pkg/tunnel/internal/netstack/icmp.go`):
- Forwards ICMP echo requests via raw socket
- Returns responses back through the gvisor stack

### 8.3 TUN Endpoints

**pkg/tunnel/server_endpoint.go**: Server-side, bridges tunnel conns to a **per-client** gvisor
`channel.Endpoint`. `readFromTCPConnWriteToEndpoint` owns role/deadline reads and
`dispatchServerDataPacket` routes registered peer traffic directly, injects cluster-bound
traffic into that client's stack, or hands packets to `serverRuntime.bridge.send`.
`readFromEndpointWriteToRoute` uses blocking, context-aware `WriteToRoutePacket`; each client
has an independent output loop, rather than a single shared loop with non-blocking drops.
Oversized stack output that cannot fit its pooled framed buffer is dropped, not truncated.
The wire framing layer's maximum remains 65535 payload bytes.

Stack grace expiry checks the expected `clientStack` identity, armed epoch, and absence of
a current data route under hub then handler locks. Control-before-data creation gets one
bounded grace interval, not one refreshed by probes; data registration invalidates it.
`clientStack.done` covers the owned output loop plus stack destruction only, not every
foreign-library goroutine. See [48-shared-server-gvisor-stack.md](48-shared-server-gvisor-stack.md).

**pkg/tunnel/client_endpoint.go**: Client-side, bridges between the local TUN device and gvisor for local traffic (src==dst).

### 8.4 Native stack diagnostics

The data-path owner sampler in `pkg/tunnel/datapath_diagnostics.go` reads selected native
counters through `gvisor_diagnostics.go` once per second. It does not install a per-segment
TCP probe or create another ticker per stack. These are embedded **netstack** statistics;
`runsc`/Sentry sandbox diagnostics do not describe these stacks. `Stack.Stats()` contains live
counter pointers, so `.Value()` is copied into a private numeric snapshot. Each counter read
is atomic; the collection is not a transactional snapshot.

| Native statistic | Meaning and limits |
|---|---|
| TCP `Retransmits`, `Timeouts` | Inner gVisor sender retransmissions and RTO expirations, not outer SSH/port-forward TCP or application deadlines. The pinned SYN/SYN-ACK retry path does not increment these counters; zero does not exclude a stalled handshake. |
| TCP `ForwardMaxInFlightDrop` | Requests rejected at the forwarder limit. `Complete` currently waits until relay termination, so this is not simply simultaneous handshakes. |
| TCP `ResetsSent`, `ResetsReceived` | Context for other anomalies; resets alone do not trigger a warning. |
| TCP `CurrentEstablished`, `CurrentConnected` | Current gauges, never unsigned-subtracted as error deltas. |
| IP `MalformedPacketsReceived`, `OutgoingPacketErrors` | Native parsing/output errors, not a complete tunnel-drop count. |
| UDP `ReceiveBufferErrors`, `MalformedPacketsReceived`, `ChecksumErrors`, `PacketSendErrors` | Native errors; receive-buffer errors also cover invalid receive state. Production RX checksum offload can bypass checksum validation. |
| `DroppedPackets` | Native transport-layer drops, not summed with overlapping protocol errors as total loss. |
| `channel.Endpoint.NumQueued()` | Current outbound packet count paired with endpoint capacity; not bytes, residence time or drops. |

Sources follow stack lifetimes with monotonic local `stack_id` values:

- `server_client`: registered by the service-owned peer registry after successful publication,
  attributed to that peer's TUN IP. Slot reconnects retaining the stack retain its ID/baseline.
- `client_interclient`: the shared inbound peer-to-local stack, not attributed to one peer.
- `client_local`: the self-to-self stack, likewise reported at stack scope.

The initial baseline is captured at registration. Canceled sources are rejected or removed,
and unregister precedes stack destruction. The sampler copies references under a short registry
lock; native reads and logs run outside route, peer and registry locks. Individual native source
cleanup and stack destruction do not wait for log output; a whole owner joining a sampler may
still wait for a permanently blocked sink.

Nonzero error/retransmission/RTO deltas emit `gvisor_stats` at Warn at most once per stack per
30 seconds. Normal idle, gauge-only and RST-only changes remain quiet. Suppressed deltas survive
rate limits, detail budgets, quiet periods and sampler gaps. Windows describe the accumulated
observation interval. Stale timestamps are rejected; a decreasing counter rebases that field
without unsigned underflow or erasing unrelated pending errors. Stage details and native
summaries share a rotating 16-detail budget; drop flushing has a separate bounded budget.

A full channel can return a short packet count without error, which the default qdisc does not
propagate: IP errors cannot reconstruct queue-full drops. Direct peer routing may bypass gVisor,
so healthy native counters do not prove progress at every routing, tunnel or TUN stage. Statistics
do not change heartbeat health or trigger reconnect. No endpoint TCPInfo/protocol probe, capture,
new HTTP endpoint, telemetry upload or global logging emitter is installed.

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
| `pkg/tunnel/internal/netstack/stack.go` | gvisor construction (`New`, `NewStack`, `NewLocalStack`) |
| `pkg/tunnel/internal/netstack/tcp.go`, `udp.go`, `icmp.go` | TCP/UDP/ICMP forwarders |
| `pkg/tunnel/server_device.go`, `client.go`, `device.go` | Server/client transport and shared device I/O |
| `pkg/tunnel/server_runtime.go`, `route_table.go`, `route_writer.go`, `bridge.go` | Runtime composition, Router/ConnList identity transactions and separate TUN bridge |
| `pkg/tunnel/server.go`, `peers.go`, `server_endpoint.go` | Connection handlers, service-owned per-IP stacks and endpoint routing |
| `pkg/tunnel/client_stack.go`, `client_endpoint.go` | Session-level local/inter-client stacks and endpoint pumps |
| `pkg/tunnel/buffered_conn.go`, `batch.go` | Joined buffered writer and packet batches |
| `pkg/internal/wire/conn.go`, `batch.go` | Shared datagram framing and batch socket writes |
| `pkg/endpoint/node.go`, `forwarder.go` | URI parsing and retry/connector/TLS dialing; `TunnelConfig` boundary |
| `pkg/endpoint/protocol_registry.go`, `servers.go`, `serve.go` | Factory registration, server assembly and joined serving lifetime |
| `pkg/endpoint/udp_relay.go` | UDP-over-TCP relay, endpoint close and copy-worker join |
| `pkg/endpoint/transporter_tcp.go`, `ssh.go` | TCP/TLS transporter and SSH protocol listener/handler |
