# KubeVPN Core Data Flow Analysis

## Overview

KubeVPN uses a TUN device + gvisor network stack to create a VPN tunnel between a local client and a remote K8s cluster. All traffic between client and server is multiplexed over a single TCP connection using a custom datagram protocol (2-byte length header + payload).

## Protocol Byte Prefix

Every packet between client and server carries a 1-byte type prefix at `data[0]`:

| Prefix | Meaning | Action at receiver |
|--------|---------|--------------------|
| `0` | Gvisor-processed packet (response from real network) | Write to TUN device directly |
| `1` | Raw IP packet (needs gvisor processing) | Inject into gvisor stack |

## Datagram Protocol (UDP-over-TCP)

TCP is a stream protocol, so packets are framed with a 2-byte big-endian length header:

```
[2 bytes: payload length][N bytes: payload (prefix byte + IP packet)]
```

Read: `readDatagramPacket` reads 2-byte length, then reads exactly N bytes.
Write: `DatagramPacket.Write` prepends the 2-byte header and writes atomically.

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
    → buf[0]==1 → inject to gvisor stack
  → gvisor TCPForwarder/UDPForwarder
    → net.Dial to real k8s service IP:port
    → bidirectional io.Copy
```

**Path 2: K8s service response → Client**
```
Real k8s service responds
  → gvisor stack processes response
  → readFromEndpointWriteToTCPConn
    → copyPacketToPool(pkt, prefix=0)
    → UDPConnOverTCP.Write (2-byte header + data)
    → TCP conn → Client
```

**Path 3: Inter-client routing (Client A → Client B)**
```
Client A sends packet to Client B's tun IP
  → readFromTCPConnWriteToEndpoint on A's conn
    → RouteMapTCP.Load(dst) → found B's conn
    → DatagramPacket.Write → B's BufferedTCP
    → B's BufferedTCP.run → B's raw TCP conn → Client B
```

**Path 4: Server TUN → Client (kernel-originated traffic)**
```
Server kernel sends IP packet to tun device
  → Device.readFromTun → tunInbound
  → Peer.routeTun
    → RouteMapTCP.Load(dst)
    → found: shift data +1 byte (add prefix=1) → DatagramPacket.Write
      → BufferedTCP → Client TCP conn
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
│  │  writeToConn ◀── tunInbound ─┘                          │ │
│  │    └─ UDPConnOverTCP.Write → TCP → server               │ │
│  │                                                         │ │
│  │  readFromConn ──▶ UDPConnOverTCP.Read                   │ │
│  │    ├─ prefix==0 ──▶ tunOutbound ──▶ writeToTun          │ │
│  │    └─ prefix==1 ──▶ gvisor stack #2                     │ │
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
  → ClientDevice.readFromTun
    → read at buf[1:], set buf[0]=1
    → parse IP: src=198.18.0.100, dst=10.96.0.1
    → src != dst → tunInbound channel
  → writeToConn
    → UDPConnOverTCP.Write (2-byte header + data)
    → TCP conn → Server
  → Server processes (Path 1 above) → response comes back
  → readFromConn
    → UDPConnOverTCP.Read
    → buf[0]==0 → tunOutbound channel
  → writeToTun
    → strip prefix byte, write IP packet to TUN
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
    → buf[0]==1 → gvisorInbound
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
  → buf[0]=1 → tunInbound → writeToConn → server
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

### Two Client Gvisor Stacks

| Stack | Created in | Purpose | Output |
|-------|-----------|---------|--------|
| #1 | `readFromTun` | Self-to-self traffic | `tunOutbound` → TUN |
| #2 | `readFromConn` | Inter-client traffic | `tunInbound` → server |

Both use `NewLocalStack` with `LocalTCPForwarder` (dials `127.0.0.1`) and `LocalUDPForwarder`.

---

## Performance Characteristics

### Per-packet overhead (client → k8s service → client roundtrip)

| Step | Operation | Cost |
|------|-----------|------|
| TUN read | syscall | ~1μs |
| Parse IP header | CPU | ~100ns |
| Channel send/recv (x4) | goroutine scheduling | ~200ns each |
| UDPConnOverTCP.Write | pool alloc + 1 memcpy | ~200ns |
| TCP write | syscall + possible TLS | ~5-50μs |
| Server gvisor inject | packet processing | ~1-5μs |
| TCPForwarder dial (first packet) | TCP handshake | ~1-100ms |
| io.Copy (bidirectional) | per-packet memcpy | ~200ns |
| Response: same path in reverse | | |

### Optimizations applied

**1. UDPConnOverTCP.Write / PacketConnOverTCP.WriteTo:**
Before: copy data to `buf[0:]`, then shift ALL data to `buf[2:]` for header.
After: copy directly to `buf[2:]`, header at `buf[0:2]`. Saves 1 memcpy per packet.

**2. Server response path (readFromEndpointWriteToTCPConn):**
Before: `copyPacketToPool` (alloc+copy) → `UDPConnOverTCP.Write` (alloc+copy) = 2 pool allocs, 2 copies.
After: single pool alloc, write `[header][prefix][data]` in one copy — bypasses UDPConnOverTCP entirely.

**3. Server TUN→client routing (routeTun):**
Before: `readFromTun` at `buf[0:]` → shift +1 → `DatagramPacket.Write` shift +2 = 3 memcpys.
After: `readFromTun` at `buf[3:]` (reserves headroom) → write header/prefix in-place = 0 extra copies.

**4. Client outbound (readFromTun → writeToConn):**
Before: `readFromTun` at `buf[1:]` → `writeToConn` → `UDPConnOverTCP.Write` (alloc + copy to `buf2[2:]`) = 1 extra alloc + 1 extra memcpy.
After: `readFromTun` at `buf[3:]` (reserves 2-byte headroom) → `writeToConn` writes header in-place at `buf[0:2]`, sends directly to raw TCP conn. Zero extra copy.

**Total: reduced from 5 memcpys to 2 per roundtrip (-60%).**

### Remaining 2 memcpys (unavoidable)

1. **`buffer.MakeWithData` in gvisor InjectInbound** — gvisor copies data into its own managed buffer. Cannot be eliminated without modifying gvisor internals.

2. **`copyPacketToPool` in server response** — copies from gvisor endpoint packet to pool buffer. Required because gvisor packet lifecycle is managed by gvisor, and we need data in a pool buffer for TCP framing.

### Remaining optimization opportunities

1. **Lazy client gvisor stack #2**: Inter-client traffic is uncommon. Deferring stack creation until first `buf[0]==1` packet from server would save ~2MB memory.

2. **BufferedTCP pool allocation**: Every inter-client write allocates a pool buffer and copies data. Could pass buffer ownership instead.

### Bug fixes: heartbeat and reconnection

**ICMP echo reply (connection stability):**
The `ICMPForwarder` previously just logged and dropped ICMP packets. Since the gvisor stack has no assigned addresses, echo replies were never generated, causing client connections to timeout every ~60s. Fixed by implementing echo reply generation via `FindRoute` + `WritePacket`. Client read deadline also increased from 1x to 3x `KeepAliveTime`.

**Immediate heartbeat on reconnection (route recovery speed):**
After reconnection, the server could not register the client's route until it received the first data packet (which triggers `AddRoute`). The heartbeat ticker fired on a fixed 60s schedule unaware of reconnection events, causing 0-58s delay before route recovery. Fixed by adding a `reconnected` channel: `handlePacket` signals it after successful dial, `heartbeats` responds by sending an immediate heartbeat and resetting the ticker. Also moved the 2s backoff sleep to only fire on dial failure.

Result: route recovery after reconnect dropped from 0-58s to < 100ms.
