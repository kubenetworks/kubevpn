# Refactoring History

## Iteration 1: RouteHub (completed)

Extracted global `RouteMapTCP` and `TCPPacketChan` into `RouteHub` struct with dependency injection.

---

## Iteration 2: Route.GenerateServers — Protocol Registry

### Current Design

`Route.GenerateServers()` uses a monolithic switch-case to dispatch protocol handling:

```go
func (r *Route) GenerateServers() ([]Server, error) {
    for _, l := range r.Listeners {
        node := ParseNode(l)
        switch node.Protocol {
        case "tun":   handler = TunHandler(...); listener = tun.Listener(...)
        case "gtcp":  handler = GvisorTCPHandler(...); listener = GvisorTCPListener(...)
        case "gudp":  handler = GvisorUDPHandler(); listener = GvisorUDPListener(...)
        case "ssh":   handler = SSHHandler(); listener = SSHListener(...)
        default:      error
        }
    }
}
```

Also:
- `Route.Retries` field is set by callers but never read anywhere — dead code.
- `ParseForwarder` hardcodes client configuration inline.

### Problem

1. **Open/Closed violation**: adding a new protocol requires modifying `GenerateServers()`
2. **Dead field**: `Retries` adds confusion with no function
3. **Mixed concerns**: parsing, dispatch, handler creation, and listener creation all in one method
4. **Repetitive error handling**: same log+return pattern repeated 4 times

### New Design

Introduce a `ProtocolFactory` function type and a registry pattern:

```
ProtocolFactory = func(node *Node, hub *RouteHub) (net.Listener, Handler, error)

protocolRegistry map[string]ProtocolFactory
```

```
Route.GenerateServers()
    │
    ├── for each listener string:
    │     ├── ParseNode(l)
    │     ├── lookup protocolRegistry[node.Protocol]
    │     └── factory(node, hub) → (Listener, Handler)
    │
    └── return []Server
```

### Migration Strategy

- Remove `Route.Retries` (dead field). Callers that set it will get a compile error — fix by removing the field assignment.
- Replace the switch-case with registry lookup.
- Each protocol registers its factory via `init()` or explicit `RegisterProtocol()`.
- Backward compat: existing exported functions (`TunHandler`, `GvisorTCPHandler`, etc.) remain unchanged.

### Files to Change

| File | Change |
|------|--------|
| `route.go` | Remove `Retries`, add registry, simplify `GenerateServers()` |
| `tunhandler.go` | Add `tunProtocolFactory` |
| `gvisortcphandler.go` | Add `gtcpProtocolFactory` |
| `gvisorudphandler.go` | Add `gudpProtocolFactory` |
| `sshhandler.go` | Add `sshProtocolFactory` (or wherever SSHHandler/SSHListener live) |
| `pkg/daemon/handler/ssh.go` | Remove `Retries: 5` |
| `pkg/daemon/action/sshdaemon.go` | Remove `Retries: 5` |
| `route_test.go` | New tests for registry and GenerateServers |

---

## Iteration 3: Remove Route Struct (completed)

Replaced `Route` struct with plain `GenerateServers(listeners []string, hub *RouteHub)` function.

---

## Iteration 4: Refactor Node and Forwarder

### Current Design

```go
type Node struct {
    Addr     string       // host:port
    Protocol string       // "tun", "gtcp", etc.
    Remote   string       // forwarding target
    Values   url.Values   // query params
    Client   *Client      // transport — set AFTER parsing, never by parser
}

type Client struct {
    Connector              // wraps conn (TCP keep-alive, etc.)
    Transporter            // dials remote (raw TCP or TLS)
}

type Forwarder struct {
    retries int
    node    *Node
}
```

Usage pattern:
```go
node, _ := ParseNode(uri)       // pure URI parse
node.Client = &Client{...}      // bolt on transport externally
fwd := NewForwarder(5, node)    // wrap in retry
conn, _ := fwd.DialContext(ctx) // retry(transport(connect))
```

### Problems

1. **Node mixes concerns**: URI identity (Addr, Protocol, Remote, Values) + runtime transport (Client)
2. **Forwarder.resolve() is dead code** — never called
3. **Client struct has no logic** — just a named pair of interfaces
4. **Forwarder.retries is always 5** — hardcoded at all call sites
5. **Node is mutable post-parse** — `Client` set externally creates temporal coupling

### New Design

Separate URI identity from transport. Remove dead code. Simplify Forwarder.

```go
// Node is pure URI parse result — immutable after construction.
type Node struct {
    Addr     string
    Protocol string
    Remote   string
    Values   url.Values
}

// Forwarder dials a remote node with retry.
type Forwarder struct {
    Addr        string       // host:port to dial
    Connector   Connector    // wraps raw conn
    Transporter Transporter  // dials transport (TCP/TLS)
    MaxRetries  int
}
```

- `Node` becomes a pure value type — no mutable transport field
- `Forwarder` directly holds `Addr` + transport interfaces — no pointer to Node
- Remove dead `resolve()` method
- Remove `Client` struct (inline its two interfaces into Forwarder)
- `ParseForwarder` constructs a `Forwarder` directly from URI + transport config

### Files to Change

| File | Change |
|------|--------|
| `node.go` | Remove `Client` field from `Node` |
| `forwarder.go` | Restructure `Forwarder` with `Addr`/`Connector`/`Transporter`/`MaxRetries`; remove `resolve()`, `getConn()`, `Node()`; remove `Client` struct |
| `route.go` | Update `ParseForwarder` to build new `Forwarder` directly |
| `protocol_registry.go` | Update `tunProtocolFactory` to pass `Node` without Client |
| `tunhandler.go` | Remove `node *Node` field if only used for passing to forwarder |
| `tunhandlerclient.go` | Update `forward.node.Remote` → `forward.Addr` in log |
| `pkg/handler/connect.go` | Build `Forwarder` directly instead of `ParseNode` + set Client |

### Current Design

```go
type Route struct {
    Listeners []string
    Hub       *RouteHub
}

func (r *Route) GenerateServers() ([]Server, error) { ... }
```

Callers:
- `server.go`: binds `route.Listeners` via cobra flag, calls `handler.Parse(*route)`
- `ssh.go`, `sshdaemon.go`: construct `Route{Listeners: [...]}`, call `handler.Parse(r)`
- `handler.Parse(r core.Route)`: just calls `r.GenerateServers()`

### Problem

1. `Hub` is never set by any caller — always falls back to `DefaultRouteHub`
2. The struct wraps `[]string` with no added behavior beyond `GenerateServers()`
3. `handler.Parse` is a trivial wrapper that adds zero value over calling `GenerateServers` directly
4. Cobra flag binding requires `&route.Listeners` — but a `[]string` var works the same way

### New Design

Replace the struct + method with a plain function:

```go
func GenerateServers(listeners []string, hub *RouteHub) ([]Server, error)
```

- Callers pass `[]string` directly
- `hub` param with nil-fallback (same as before)
- Remove `handler.Parse` wrapper — callers call `core.GenerateServers` directly
- Cobra binds to a plain `[]string` variable

### Files to Change

| File | Change |
|------|--------|
| `route.go` | Remove `Route` struct, convert to `func GenerateServers(listeners []string, hub *RouteHub)` |
| `pkg/handler/connect.go` | Remove `Parse()` function, inline into callers or update callers |
| `cmd/kubevpn/cmds/server.go` | Use `[]string` var + call `core.GenerateServers` directly |
| `pkg/daemon/handler/ssh.go` | Call `core.GenerateServers(listeners, nil)` |
| `pkg/daemon/action/sshdaemon.go` | Call `core.GenerateServers(listeners, nil)` |
| `route_test.go` | Update tests to use new function signature |

---

## Iteration 5: Deduplicate Client/Server Code

### Current Design

The client and server sides each have their own gvisor stack, TCP forwarder, and UDP forwarder implementations. These are near-identical copies differing only in how they resolve destination addresses.

### Problems

1. **Duplicated stack creation** (104 lines x 2): `NewStack()` and `NewLocalStack()` differ only in TCP/UDP forwarder registration
2. **Duplicated UDP conn type**: `gvisorUDPConnOverTCP` is identical to `UDPConnOverTCP`
3. **Duplicated TCP forwarder** (~75 lines x 2): `TCPForwarder()` vs `LocalTCPForwarder()` differ only in address resolution
4. **Duplicated UDP forwarder** (~120 lines x 2): `UDPForwarder()` vs `LocalUDPForwarder()` differ only in address resolution

### New Design

1. **Parameterized stack**: `newGvisorStack(ctx, tun, tcpFwd, udpFwd)` with thin wrappers
2. **Remove duplicate type**: delete `gvisorUDPConnOverTCP`, use `UDPConnOverTCP` everywhere
3. **Parameterized TCP forwarder**: `newTCPForwarder(ctx, s, resolveAddr, ctxOverride)` with thin wrappers
4. **Parameterized UDP forwarder**: `newUDPForwarder(ctx, s, resolveAddr)` with thin wrappers

### Files to Change

| File | Change |
|------|--------|
| `gvisorstack.go` | Add `newGvisorStack`, keep `NewStack` as wrapper, add `NewLocalStack` wrapper |
| `gvisorlocalstack.go` | Delete (merged into `gvisorstack.go`) |
| `gvisortcpforwarder.go` | Add `newTCPForwarder`, keep `TCPForwarder`/`LocalTCPForwarder` as wrappers |
| `gvisorlocaltcpforwarder.go` | Delete (merged into `gvisortcpforwarder.go`) |
| `gvisorudpforwarder.go` | Add `newUDPForwarder`, keep `UDPForwarder`/`LocalUDPForwarder` as wrappers |
| `gvisorlocaludpforwarder.go` | Delete (merged into `gvisorudpforwarder.go`) |
| `gvisortunendpoint.go` | Use `NewUDPConnOverTCP` instead of `newGvisorUDPConnOverTCP` |
| `gvisorudphandler.go` | Remove `gvisorUDPConnOverTCP` type and `newGvisorUDPConnOverTCP` |

---

## Iteration 6: Data Flow Analysis and Performance

### Protocol Byte Prefix

All packets between client and server carry a 1-byte type prefix:
- `0` = gvisor-processed packet (response from real network)
- `1` = raw IP packet (needs gvisor processing at destination)

### Server Data Flow (cmd/kubevpn/cmds/server.go)

Listeners: `tun://?net=...` + `gtcp://:10801`

```
Client request path:
  gtcp TCP conn → readFromTCPConnWriteToEndpoint
    → UDPConnOverTCP.Read (parse 2-byte datagram header)
    → Parse IP header, update RouteMapTCP
    → if buf[0]==1: inject to gvisor stack
    → gvisor TCPForwarder/UDPForwarder dials real k8s service

Server response path:
  gvisor endpoint → readFromEndpointWriteToTCPConn
    → copyPacketToPool (prefix 0)
    → UDPConnOverTCP.Write (2-byte header + data, single copy)
    → TCP conn back to client

Inter-client routing:
  Client A packet → readFromTCPConnWriteToEndpoint
    → RouteMapTCP lookup for dst
    → if found: DatagramPacket.Write → B's BufferedTCP → B's TCP conn

TUN device path (less common):
  Server kernel → Device.readFromTun → tunInbound
    → Peer.routeTun → RouteMapTCP lookup
      → if found: shift + DatagramPacket.Write → BufferedTCP
      → if not found: drop
  Peer.routeTCPToTun ← TCPPacketChan → tunOutbound → TUN
```

### Client Data Flow (pkg/handler/connect.go:386)

Listener: `tun://` with forwarder to server's gtcp port

```
Outbound (local app → k8s):
  App → TUN → ClientDevice.readFromTun
    → buf[0]=1, parse IP
    → if src==dst: gvisorInbound (local gvisor stack #1)
    → else: tunInbound → writeToConn → UDPConnOverTCP.Write → TCP → server

Inbound (k8s → local app):
  TCP → readFromConn → UDPConnOverTCP.Read
    → if buf[0]==0: tunOutbound → writeToTun → TUN → App
    → if buf[0]==1: gvisorInbound (local gvisor stack #2)

Local gvisor stack #1: handles self-to-self traffic (e.g. ping own tun IP)
  → output goes to tunOutbound → TUN device

Local gvisor stack #2: handles inter-client traffic (other clients → local services)
  → LocalTCPForwarder dials 127.0.0.1:port
  → output goes to tunInbound → writeToConn → back to server for routing
```

### Performance Optimization Applied

**Eliminated redundant memcpy in UDPConnOverTCP.Write and PacketConnOverTCP.WriteTo:**

Before: data copied to buf[0:], then DatagramPacket.Write shifted all data right by 2 bytes.
After: data copied directly to buf[2:], header written at buf[0:2]. One copy instead of two.

### Remaining Optimization Opportunities

1. **Client `UDPConnOverTCP.Write`**: Still has 1 extra pool alloc + memcpy. Fix: `readFromTun` reads at `buf[3:]` (same strategy as server), but requires changing `writeToTun` data format handling — higher risk.

2. **Lazy gvisor stack #2 on client**: Inter-client stack always created but rarely used. Deferring to first `buf[0]==1` packet saves ~2MB memory.

---

## Iteration 7: Node Schema and Builder API (completed)

- Added comprehensive Node URI schema documentation (supported protocols and parameters)
- Added `NewNode(protocol, addr)` constructor with chainable `WithForward()` / `WithParam()` setters
- Added `GenerateServersFromNodes([]*Node, *RouteHub)` for programmatic construction
- Callers (`sshdaemon.go`, `ssh.go`) updated from `fmt.Sprintf` URI strings to builder API
- `ParseNode` now validates protocol scheme (rejects missing scheme)
- Renamed `Node.Remote` → `Node.Forward` for clarity

---

## Iteration 8: Extract Shared tunDevice and Buffer Helper (completed)

- Created `tundevice.go` with shared `tunDevice` struct (fields + `writeToTun` + `Close`)
- `Device` (server) and `ClientDevice` (client) embed `tunDevice`, eliminating duplicate code
- Moved `Packet`, `NewPacket`, `MaxSize` to `tundevice.go` (shared by both sides)
- Added `copyPacketToPool()` helper for gvisor packet → pool buffer copy pattern

---

## Iteration 9: Design and Code Elegance Improvements (completed)

### Design
- Moved `addToRouteMapTCP` / `removeFromRouteMapTCP` to `RouteHub` as `AddRoute()` / `RemoveRoutesByConn()` — route logic belongs with routing state, not TCP handler
- Fixed data race in `connbufferedtcp.go`: bare `bool` → `atomic.Bool`
- Renamed `handle()` → `relayUDPOverTCP()` for clarity

### Code Style
- Fixed reversed naming: `cancel, cancelFunc` → `ctx, cancel` (Go convention)
- Inlined trivial wrappers: `forwardConn()` (was just `DialContext`), `Peer.sendErr()` (used once)
- Extracted `sendHeartbeat` closure in heartbeats to remove IPv4/IPv6 duplication
- Renamed exported `Chan`/`Run` → `ch`/`run` in `bufferedTCP` (unexported type)

---

## Iteration 10: Simplify Channel-Drain Loops (completed)

Replaced 6 instances of verbose pattern:
```go
for ctx.Err() == nil {
    var packet *Packet
    select {
    case packet = <-ch:
        if packet == nil { return }
    case <-ctx.Done():
        return
    }
    // process
}
```

With idiomatic Go:
```go
for {
    select {
    case packet := <-ch:
        if packet == nil { return }
        // process inline
    case <-ctx.Done():
        return
    }
}
```

Benefits: removes redundant `ctx.Err()` check, uses `:=` in case, processes inline.

---

## Iteration 11: Performance Optimizations (completed)

### 11a: UDPConnOverTCP.Write / PacketConnOverTCP.WriteTo

Before: copy data to `buf[0:]`, then `DatagramPacket.Write` shifts ALL data right by 2 bytes.
After: copy data directly to `buf[2:]`, header at `buf[0:2]`. Saves 1 memcpy per packet.

### 11b: Server Response Path (readFromEndpointWriteToTCPConn)

Before: `copyPacketToPool` (alloc A + copy) → `UDPConnOverTCP.Write` (alloc B + copy to B[2:]).
After: single pool alloc, write `[2-byte header][1-byte prefix][data]` in one copy.
Saves: 1 memcpy + 1 pool alloc per response packet.

### 11c: Server TUN→Client Routing (routeTun)

Before: `readFromTun` at `buf[0:]` → `routeTun` shifts +1 → `DatagramPacket.Write` shifts +2 = **3 memcpys**.
After: `readFromTun` at `buf[3:]` (reserves headroom) → `routeTun` writes header at `buf[0:2]`, prefix at `buf[2]` → single write. **0 extra memcpys**.

### Summary

| Path | Before | After | Saved |
|------|--------|-------|-------|
| UDPConnOverTCP.Write (both sides) | 2 copies | 1 copy | -1 |
| Server response (gvisor→client) | 2 copies + 2 allocs | 1 copy + 1 alloc | -1 copy, -1 alloc |
| Server TUN routing (TUN→client) | 3 copies | 1 copy | -2 |
| **Total per roundtrip** | **5 copies** | **3 copies** | **-40%** |

---

## Iteration 12: Bug Fixes — ICMP Heartbeat and Connection Stability (completed)

### Problem

Clients periodically lost access to cluster service IPs. Server logs showed:
- Client routes deleted every ~108s (EOF on TCP conn)
- Reconnection gap of ~12s each time
- Zero ICMP echo replies in logs despite heartbeats being sent

### Root Cause

1. Client sends ICMP heartbeats every 60s to `198.18.0.0` (RouterIP)
2. Server gvisor stack has **no assigned IP address** (promiscuous mode only)
3. Gvisor never generates echo replies for addresses it doesn't "own"
4. `ICMPForwarder` just logged and dropped all ICMP — no forwarding
5. Client `readFromConn` read deadline = 60s = same as heartbeat interval → race condition

### Fix

1. **ICMP echo reply**: Reimplemented `ICMPForwarder` to construct ICMP echo reply directly in gvisor stack via `FindRoute` + `WritePacket`. Supports both ICMPv4 and ICMPv6.
2. **Read deadline**: Increased from `1x KeepAliveTime` to `3x KeepAliveTime` as safety margin.

---

## Iteration 13: Unified Log Style (completed)

### Tag Convention

| Tag | Scope |
|-----|-------|
| `[TUN]` | TUN device I/O |
| `[Client]` | Client-side operations |
| `[Gvisor-TCP]` | Gvisor TCP forwarder / handler / endpoint |
| `[Gvisor-UDP]` | Gvisor UDP forwarder |
| `[Gvisor-ICMP]` | Gvisor ICMP handler |
| `[UDP]` | UDP relay handler |
| `[Route]` | Route hub operations |
| `[Transport]` | TLS / TCP transport |
| `[SSH]` | SSH tunnel |
| `[TCP]` | Buffered TCP internal |

### Fixes Applied

- Added `[Tag]` prefixes to ~20 untagged messages
- Fixed missing colons: `"RemoteAddress %s"` → `"RemoteAddress: %s"`
- Fixed grammar: `"Find"` → `"Found"`, `"Write length"` → `"Wrote"`, `"drop it"` → `"dropping"`
- Fixed format verbs: `%s` / `err.Error()` → `%v` / `err`
- Fixed lowercase starts, `Infoln` → `Infof`
- Simplified `plog.G(plog.WithFields(context.Background(), plog.GetFields(ctx)))` → `plog.G(ctx)`

---

## Iteration 14: Immediate Heartbeat on Reconnection (completed)

### Problem

After a client reconnects, the server cannot register its route until it receives the first data packet containing the client's source IP (triggers `AddRoute`). The heartbeat fires on a fixed 60s ticker that is NOT aware of reconnection events.

### Evidence from logs

Client 198.18.0.2 first packet after reconnect was ALWAYS at `:46.955` — the heartbeat's fixed schedule — regardless of when the disconnect occurred:

```
Disconnect 09:17:16 → First packet 09:17:46 (30s wait)
Disconnect 09:18:18 → First packet 09:18:46 (28s wait)
Disconnect 09:19:20 → First packet 09:19:46 (26s wait)
Disconnect 09:20:22 → First packet 09:20:46 (24s wait)
```

### Root Cause

The delay is NOT a performance issue. It is the time between reconnection and the next heartbeat ticker fire:

```
Disconnect → 2s fixed sleep → TCP dial (fast) → wait 0-58s for heartbeat → AddRoute
```

### Fix

1. Added `reconnected` channel to `ClientDevice`
2. `handlePacket` signals `reconnected` after successful dial
3. `heartbeats` goroutine listens on the channel and sends an immediate heartbeat + resets the ticker
4. Moved the 2s backoff sleep from `defer` (runs every time) to only after dial failure

### Result

```
Before: disconnect → 2s sleep → dial → wait 0-58s for heartbeat → AddRoute
After:  disconnect → dial → immediate heartbeat → AddRoute (< 100ms)
```

---

## Iteration 15: Client Outbound Zero-Copy (completed)

### Problem

Client outbound path still had 1 unnecessary memcpy + 1 pool alloc per packet:

```
readFromTun: reads at buf[1:], prefix at buf[0]
  → tunInbound channel
  → writeToConn → UDPConnOverTCP.Write:
      buf2 = pool.Get()               ← extra pool alloc
      copy(buf2[2:], buf[0:n+1])      ← extra memcpy (~1500B)
      header at buf2[0:2]
      conn.Write(buf2[:n+3])
      pool.Put(buf2)
```

### Fix

Same strategy as server-side routeTun (Iteration 11c): reserve headroom in the pool buffer.

1. `readFromTun`: read at `buf[3:]`, prefix at `buf[2]`, leaving `buf[0:2]` free for datagram header
2. `writeToConn`: write header in-place at `buf[0:2]`, send `buf[0:length+2]` directly to raw TCP conn — no extra alloc, no extra copy
3. `sendHeartbeat`: same headroom convention (`buf[3:]`, prefix at `buf[2]`)
4. `copyPacketToPool`: added `headroom` parameter for output buffer offset
5. `handleGvisorPacket`: added `headroom` parameter — `0` for tunOutbound (writeToTun path), `2` for tunInbound (writeToConn path)
6. Self-to-self path (rare, src==dst): shifts data back to offset 0 for gvisor compatibility

### Result

```
Before: readFromTun → tunInbound → UDPConnOverTCP.Write (alloc + copy) → TCP
After:  readFromTun (headroom) → tunInbound → writeToConn (in-place header) → TCP
```

Saved: 1 pool alloc + 1 memcpy per outbound packet on the hottest client path.

---

## Performance Summary (all iterations)

### Memcpy per request-response roundtrip

| Path | Initial | After optimization |
|------|---------|-------------------|
| Client outbound (TUN → remote) | 1 | **0** |
| Server inject to gvisor | 1 | 1 (unavoidable, gvisor internal) |
| Server response (gvisor → client) | 2 | **1** |
| Server TUN→client routing | 3 | **0** |
| Client inbound (remote → TUN) | 0 | 0 |
| **Total per roundtrip** | **5** | **2** |
| **Reduction** | | **-60%** |

### Remaining 2 memcpys (unavoidable)

1. `buffer.MakeWithData` in gvisor InjectInbound — gvisor copies data into its own managed buffer
2. `copyPacketToPool` in server response — copies from gvisor endpoint packet to pool buffer (gvisor packet lifecycle requires this)
