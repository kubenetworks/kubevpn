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

### Remaining Optimization Opportunities (not implemented — higher risk)

1. **Peer.routeTun shift + DatagramPacket.Write shift**: Server shifts packet data right by 1, then DatagramPacket shifts by 2. Could be avoided if readFromTun reads at offset 3. Risk: changes buffer layout convention.

2. **Lazy gvisor stack initialization on client**: Two gvisor stacks are always created. Stack #2 (inter-client) is only used when other clients connect to local services. Could defer creation until first packet. Risk: startup latency on first inter-client packet.
