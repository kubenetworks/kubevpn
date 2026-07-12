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
│               │            │ ipHash(dst)    │     │        │           │                      │
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
- `tun://?net=198.18.0.100/16` — create TUN with specified IP
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

### 5.2 Client Side (`ClientDevice`)

```
HandleClient:
  readFromTun ──► tunInbound ──► runConnPool ──► slots[hash(dst)] ──► writeToConn ──► server
                                                                                        │
  writeToTun  ◄── tunOutbound ◄──────────────── readFromConn ◄──────────────────────────┘
```

**Connection pool**: The client maintains `ConnPoolSize=4` parallel TCP connections to the server. Packets are distributed by `ipHash(dst)` — a FNV-1a hash of the destination IP, ensuring all packets for the same destination use the same connection (preserving ordering).

**Heartbeats**: Periodic ICMP packets sent to all connections to keep routes alive in the server's RouteHub.

**Reconnection**: Each slot reconnects independently on failure. After reconnection, an immediate heartbeat is sent to re-register routes.

## 6. RouteHub — Packet Routing

`RouteHub` is the central routing table shared between TUN and gvisor handlers:

```go
type RouteHub struct {
    RouteMapTCP   *sync.Map     // map[string]*ConnList — dstIP → connections
    TCPPacketChan chan *Packet   // gvisor endpoint → TUN bridge
}
```

**ConnList**: Holds multiple connections for the same client IP (connection pool). `Write()` tries each connection, removing dead ones on failure — providing automatic failover.

**Route registration**: When the server receives a heartbeat (ICMP packet), it calls `AddRoute(srcIP, conn)` to register the client's IP in the routing table.

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

Wraps a TCP connection with buffered writes for improved throughput.

## 8. Gvisor Stack

### 8.1 Stack Creation

`newGvisorStack(ctx, tun, tcpFwd, udpFwd)` creates a gvisor network stack with:
- IPv4 + IPv6 network protocols
- TCP + UDP transport protocols
- SACK, TTL=64, moderate receive buffer, forwarding enabled
- Promiscuous mode + spoofing enabled (accepts all packets)
- Default routes for all IPv4 and IPv6 traffic

Two variants:
- `NewStack()` — TCP/UDP forwarded to real destinations (production sidecar)
- `NewLocalStack()` — forwarded to localhost (dev/test mode)

### 8.2 Transport Forwarders

**TCPForwarder** (`gvisor_tcp_forwarder.go`):
- Intercepts TCP SYN from gvisor stack
- Dials the real destination (e.g., the actual service IP)
- Relays data bidirectionally between gvisor gonet.Conn and the real TCP conn

**UDPForwarder** (`gvisor_udp_forwarder.go`):
- Intercepts UDP packets from gvisor stack
- Dials the real destination UDP address
- Relays with configurable timeouts (5-minute idle)

**ICMPForwarder** (`gvisor_icmp_forwarder.go`):
- Forwards ICMP echo requests via raw socket
- Returns responses back through the gvisor stack

### 8.3 TUN Endpoints

**gvisor_tun_endpoint.go**: Server-side, bridges between a TCP connection (from VPN client) and a gvisor `channel.Endpoint`. Reads datagram-framed packets from the TCP conn and injects them into gvisor. Reads packets from gvisor and writes them back over TCP.

**gvisor_local_tun_endpoint.go**: Client-side, bridges between the local TUN device and gvisor for local traffic (src==dst).

## 9. Sidecar IP Allocation

The `tun` protocol factory handles sidecar self-DHCP:
1. If `net=` parameter is empty, call `requestTunIPFromControlPlane()` → gRPC `GetTunIP` to TunConfigServer
2. Create the TUN device with the returned IP

The factory fetches the IP **once at startup** and does not run any background watcher. The sidecar's own TUN IP is fixed for the process lifetime; runtime routing changes (when a *client's* IP changes) are handled by the control-plane (`syncEnvoyRuleIP`) and pushed to the sidecar's envoy via xDS — not by the sidecar editing its own device. The former `watchTunIPChanges` / `pollTunIP` / `applyTunIPChange` + `tun.UpdateDNAT` path was removed by the unified proxy mode (see [09-tun-ip-hot-update.md](09-tun-ip-hot-update.md) and [29-sleep-wake-ip-update.md](29-sleep-wake-ip-update.md)).

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
