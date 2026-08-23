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
│               │            │ flowHash(5tup) │     │        │           │                      │
│               │            ▼                │     │        ▼           │                      │
│          tunOutbound    slots[0..3]          │     │     RouteHub ──► tunOutbound              │
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

`tunHandler.Handle()` checks if a `Forwarder` is configured:
- **No forwarder** → `HandleServer()` — this is the traffic manager
- **Has forwarder** → `HandleClient()` — this is the local machine or sidecar

### 5.1 Server Side (`Device`)

```
HandleServer:
  readFromTun ──► tunInbound ──► Peer.routeTun ──► RouteHub.WriteToRoute ──► remote conn
                                                                                │
  writeToTun  ◄── tunOutbound ◄── Peer.routeTCPToTun ◄── RouteHub.TCPPacketChan ┘
```

The server reads IP packets from the TUN device, looks up the destination in `RouteHub`, and writes to the appropriate remote connection. Incoming packets from remote connections are written back to the TUN device.

**Packet framing**: IP data is read at `buf[3:]`, leaving room for `[2-byte length][1-byte type prefix]`. The type prefix byte (`1` = IP packet) distinguishes different payload types.

**Shared read loop**: both server and client read from the TUN via one generic loop,
`tunDevice.pumpTun(ctx, errLabel, dispatch)` (in `tun_device.go`). It owns the pooled-buffer
read at `buf[3:]`, `ParseIPFast`, and read/parse error handling (read error → `errChan` + stop;
parse error → drop + continue), then hands each packet to a per-side `dispatch` callback.
`Device.readFromTun` is just `pumpTun(ctx, "[TUN]", …)` whose dispatch debug-logs and sends to
`tunInbound`.

### 5.2 Client Side (`ClientDevice`)

```
HandleClient:
  readFromTun ──► tunInbound ──► runConnPool ──► slots[flowHash(5tup)] ──► writeToConn ──► server
                                                                                        │
  writeToTun  ◄── tunOutbound ◄──────────────── readFromConn ◄──────────────────────────┘
```

`ClientDevice.readFromTun` uses the same `pumpTun` loop as the server; its dispatch sets the
type prefix and either forwards locally (when `src == dst`, via the gvisor handler) or pushes
to `tunInbound` for the pool to distribute.

**Connection pool**: The client maintains `ConnPoolSize=4` parallel TCP connections to the server. Packets are distributed by `flowHash` — a stateless FNV-1a hash of the flow's five-tuple `(proto, dst IP, src port, dst port)` — so distinct connections to the same hot destination IP spread across slots, while every packet of one flow stays on the same connection (a correctness requirement: the server runs one gvisor stack per conn). Packets with no usable ports (ICMP, IP fragments, unparsed IPv6 extension headers) fall back to `ipHash(dst)`. See [41-five-tuple-pool-dispatch.md](41-five-tuple-pool-dispatch.md).

**`connSlot`**: each of the `ConnPoolSize` slots is a `connSlot` value that owns its inbound
channel and connection lifecycle — `run` (dial + reconnect loop), `readFromConn`, and
`writeToConn` are methods on it (rather than free functions threading a `slotID`). `runConnPool`
builds the `[]*connSlot`, starts each `go slot.run(ctx)`, and routes inbound packets:
`dst != nil → trySendToSlot(slots[flowHash(5-tuple)], pkt)`, else `broadcastToSlots(slots, pkt)` for
heartbeats. Both `readFromConn` and `writeToConn` refresh their conn deadline lazily via a
shared `periodicDeadline` (one `SetDeadline` syscall per half-timeout instead of per packet).

**Heartbeats**: Periodic ICMP packets sent to all connections to keep routes alive in the server's RouteHub.

**Reconnection**: Each slot reconnects independently on failure. After reconnecting, the slot proactively announces its TUN IP directly on the new conn (`registrationPayloads`), so the server re-registers the route immediately — no waiting for the next periodic heartbeat.

### 5.3 File layout

| File | Responsibility |
|------|----------------|
| `packet.go` | The `Packet` value type (`NewPacket`/`Data`/`Length`), the gvisor `copyPacketToPool` helper, and `logIPPacket` (the debug packet-log helper — the only `gopacket/layers` user among these files) |
| `tun_device.go` | Shared `tunDevice` base (`tun`, the inbound/outbound channels, `errChan`), `writeToTun`, `Close`, the generic `pumpTun` read loop, and `drainPacketChan` |
| `tun_client.go` | `ClientDevice`, the connection pool (`runConnPool`, `flowHash`/`parseFiveTupleInline`/`ipHash`, `trySendToSlot`/`broadcastToSlots`, `periodicDeadline`), and the `connSlot` type |
| `tun_server.go` | `Device`, `Peer` routing (`routeTun`/`routeTCPToTun`), and `tunHandler` dispatch |

## 6. RouteHub — Packet Routing

`RouteHub` is the central routing table shared between TUN and gvisor handlers:

```go
type RouteHub struct {
    RouteMapTCP   *sync.Map     // map[string]*ConnList — dstIP → connections
    TCPPacketChan chan *Packet   // gvisor endpoint → TUN bridge
}
```

**ConnList**: Holds multiple connections for the same client IP (connection pool), ordered newest-first (`Add` prepends). `Write()` tries each connection from the head, removing dead ones on failure — automatic failover that also keeps reverse traffic on the freshest conn after a reconnect.

**Route registration**: When the server receives a packet from a client (a data packet, the per-conn registration packet a slot sends right after dialing, or a periodic heartbeat), it calls `AddRoute(srcIP, conn)` to register the client's IP. Stale "ghost" conns are evicted by the server-side read/write deadlines (see [08-heartbeat-health.md](08-heartbeat-health.md) §#3).

## 7. Protocol Encapsulation

### 7.1 UDPOverTCP (`conn_udp_over_tcp.go`)

Provides UDP-like datagram semantics over a TCP stream using length-prefix framing:

```
[2-byte big-endian length][payload]
[2-byte big-endian length][payload]
...
```

Used by the TUN client→server connection to preserve packet boundaries.

### 7.2 PacketConnOverTCP (`conn_packet_over_tcp.go`)

Same length-prefix framing but implements `net.PacketConn` for compatibility with code expecting `ReadFrom`/`WriteTo`.

### 7.3 BufferedTCP (`conn_buffered_tcp.go`)

Wraps a TCP connection with an **async write queue** to coalesce syscalls under load.
`NewBufferedTCP` starts a single `run(ctx)` goroutine that drains a buffered channel
(`ch chan *Packet`, capacity `MaxSize`) and writes each framed packet to the socket. Two enqueue
paths:

- **`writePacket(pkt)`** — the hot path. Enqueues an *already-framed* `Packet` and **transfers one
  reference**: `run` calls `pkt.release()` after the socket write. Returns `false` if the conn is
  closed, in which case ownership is **not** taken (caller keeps its ref). This is zero-copy.
- **`Write(b)`** — the `net.Conn` contract path. Because the caller may reuse `b` after `Write`
  returns, `b` is **copied** into a pooled buffer. Hot routing paths therefore prefer
  `writePacket`.

A `closed atomic.Bool` plus a `done` channel (closed when `run` exits) ensure `writePacket`/`Write`
never block on a drainerless channel after shutdown.

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
copying** — e.g. heartbeat fan-out across all conn slots (`broadcastToSlots`) or an async
`bufferedTCP` queue:

- `refs` counts *extra* references beyond the implicit owner, so the zero value (a plain
  `&Packet{}`) is a valid single-owner packet freed by exactly one `release()`.
- `acquire()` adds a reference; `release()` drops one and returns the buffer to `config.LPool` when
  the implicit owner's ref is dropped (`refs` reaches `-1`).
- Releasing more times than acquired **panics** (`released more times than acquired`) — a
  use-after-free guard rather than silent corruption.

This is modeled on gvisor's chunk refcount (`buffer.View.Clone`/`Release`). Type-prefix values
`2..255` are reserved for future control/heartbeat frames.

## 8. Gvisor Stack

### 8.1 Stack Creation

`newGvisorStack(ctx, tun, tcpFwd, udpFwd)` creates a gvisor network stack with:
- IPv4 + IPv6 network protocols
- TCP + UDP transport protocols
- SACK, TTL=64, moderate receive buffer, forwarding enabled
- Stock gvisor TCP send/receive buffer ranges (autotuning up to 4MB via `ModerateReceiveBuffer`). NOTE: raising these to Tailscale's 8MiB/6MiB was tried and reverted — Tailscale assumes one UDP flow per peer, but KubeVPN multiplexes every flow over a shared TCP tunnel, where an oversized single-flow buffer causes head-of-line bloat that starves other flows (see `08-heartbeat-health.md` §#3)
- `GVisorGSOSupported` on the link endpoint — gvisor performs software segmentation internally, complementing the TUN-side GRO (`docs/22-tun-device.md` §4.2). We deliberately avoid `HostGSOSupported`: this endpoint feeds a TCP tunnel, not a host NIC, so host-GSO would push super-MTU segments onto the wire that macOS/Windows clients cannot write to their TUN
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

**gvisor_tun_endpoint.go**: Server-side, bridges between a TCP connection (from VPN client) and a gvisor `channel.Endpoint`. Reads datagram-framed packets from the TCP conn and injects them into gvisor. Reads packets from gvisor and writes them back over TCP. The send path (`readFromEndpointWriteToTCPConn`) carries a frame-boundary guard symmetric with the TUN read guard: a packet too large to fit the `[2 len][1 type][IP]` frame is dropped rather than truncated (TCP recovers). With `GVisorGSOSupported` the endpoint only sees ≤MTU packets, so this is defense-in-depth.

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
| `tun_server.go` | Server-side Device, Peer, routing |
| `tun_client.go` | Client-side ClientDevice, connection pool, heartbeats |
| `tun_device.go` | Shared tunDevice base, Packet type |
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
| `conn_packet_over_tcp.go` | PacketConn-over-TCP framing |
| `conn_buffered_tcp.go` | Buffered TCP wrapper |
| `transporter_tcp.go` | TCP Transporter with optional TLS |
| `ssh.go` | SSH protocol listener/handler |
