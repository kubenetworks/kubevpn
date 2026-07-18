# Local Proxy Design

## 1. Overview

The localproxy package (`pkg/localproxy`) provides a local SOCKS5 and HTTP CONNECT proxy server that forwards traffic to the Kubernetes cluster. This enables applications that don't support VPN/TUN interfaces (or run in environments where TUN isn't available) to access cluster services through standard proxy protocols.

## 2. Architecture

```
Local Application
  │
  ├── SOCKS5 proxy ──────────┐
  │   (socks5://localhost:X)  │
  │                           ▼
  └── HTTP CONNECT proxy ──► ClusterConnector
      (http://localhost:Y)       │
                                 ├── resolve service name
                                 │   (Service → Endpoints → Pod)
                                 │
                                 ├── select target pod
                                 │   (random from ready endpoints)
                                 │
                                 └── kubectl port-forward
                                     (PodDialer → net.Conn)
```

## 3. Components

### 3.1 Server (`server.go`)

`Server` runs one or both proxy listeners:

```go
type Server struct {
    Connector         Connector        // how to reach the cluster
    SOCKSListenAddr   string           // e.g. "127.0.0.1:1080"
    HTTPConnectListen string           // e.g. "127.0.0.1:8080"
}
```

`ListenAndServe(ctx)` starts configured listeners concurrently and blocks until context cancellation. Graceful shutdown: cancels context → closes listeners → waits for goroutines.

### 3.2 SOCKS5 (`socks5.go`)

Standard SOCKS5 proxy (RFC 1928):
- Auth method: No authentication (`0x00`)
- Command: CONNECT only (`0x01`)
- Address types: IPv4, FQDN, IPv6
- On successful connect: relay bidirectionally between client and cluster connection

### 3.3 HTTP CONNECT (`httpconnect.go`)

Standard HTTP CONNECT proxy (RFC 7231):
- Reads HTTP CONNECT request
- Validates method (only CONNECT allowed)
- Dials cluster connection via Connector
- Returns `200 Connection Established`
- Relays bidirectionally

### 3.4 Relay (`relay.go`)

`relayConns(left, right)` — bidirectional `io.Copy` between two `net.Conn`s with `CloseWrite` on TCP connections for clean shutdown.

### 3.5 Connector Interface

```go
type Connector interface {
    Connect(ctx context.Context, host string, port int) (net.Conn, error)
}
```

### 3.6 ClusterConnector (`cluster.go`)

The production `Connector` implementation that resolves cluster addresses:

```go
type ClusterConnector struct {
    Client           ClusterAPI   // K8s service/endpoint lookups
    Forwarder        PodDialer    // kubectl port-forward
    RESTConfig       *rest.Config
    DefaultNamespace string
}
```

**Resolution flow for `Connect(ctx, host, port)`:**
1. Parse host as `service`, `service.namespace`, or `service.namespace.svc.cluster.local`
2. Look up K8s Service → find Endpoints → pick a random ready pod
3. Map service port to target pod port
4. `PodDialer.DialPod(ctx, namespace, podName, podPort)` → kubectl port-forward

### 3.6b HostConnector (`host.go`)

An alternative `Connector` that dials targets **directly from the host** using the host's own
DNS resolver and network stack (`net.Dialer.DialContext`), bounded by `config.DialTimeout`:

```go
type HostConnector struct { dialer net.Dialer }
func (c *HostConnector) Connect(ctx, host, port) (net.Conn, error) // net.Dial host:port
```

Combined with the servers' hostname pass-through (socks5h), it reaches:
- the **public internet** via the host's default route, and
- **cluster** Services/Pods too, **when a KubeVPN TUN is active** (the TUN routes + cluster DNS
  are installed host-wide, so `net.Dial` to a ClusterIP / cluster name is transparently tunneled).

Unlike `ClusterConnector` it does **no** Kubernetes API lookup and **no** port-forward, so it needs
no cluster client. Selected by `proxy-out --egress` and by `connect --socks --egress`.

### 3.7 PodDialer (`portforward.go`)

```go
type PodDialer interface {
    DialPod(ctx context.Context, namespace, podName string, port int32) (net.Conn, error)
}
```

`kubePortForwarder` implementation:
1. Listen on a random local TCP port
2. Start `util.PortForwardPod()` in background (kubectl port-forward)
3. Wait for ready signal
4. Dial `127.0.0.1:<localPort>` → returns `net.Conn`

### 3.8 ClusterAPI (`cluster.go`)

Abstracts K8s API calls for testability:

```go
type ClusterAPI interface {
    GetService(ctx, namespace, name) (*Service, error)
    GetEndpoints(ctx, namespace, name) (*Endpoints, error)
    ListServices(ctx) (*ServiceList, error)
    ListPodsByIP(ctx, ip) (*PodList, error)
}
```

## 4. Related Files

| File | Purpose |
|------|---------|
| `pkg/localproxy/server.go` | Server, ListenAndServe |
| `pkg/localproxy/socks5.go` | SOCKS5 proxy handler |
| `pkg/localproxy/httpconnect.go` | HTTP CONNECT proxy handler |
| `pkg/localproxy/relay.go` | Bidirectional connection relay |
| `pkg/localproxy/cluster.go` | ClusterConnector, ClusterAPI, service resolution |
| `pkg/localproxy/host.go` | HostConnector — direct host-network dial (socks5h egress) |
| `pkg/localproxy/portforward.go` | PodDialer, kubectl port-forward |

## 5. Invocation modes

The proxy is exposed two ways:

### 5.1 `kubevpn proxy-out` (standalone, no daemon)

Runs the `Server` in the foreground of the CLI process — no daemon, no TUN, no privileges.
`--egress` selects `HostConnector`; otherwise `ClusterConnector`. Designed for nested-VPN cases
where OS routes are owned by another VPN client, or for pure host egress.

### 5.2 `kubevpn connect --socks` (user-daemon-managed)

`connect --socks` (optionally `--egress`) sets `ConnectRequest.{EnableSocks,SocksListen,SocksEgress}`.
The **user daemon** starts the `Server` as an in-process goroutine once the connection is live
(`forwardConnectToSudo` → `startManagedSocksProxy`), bound to the connection's `SessionLifecycle.Ctx`
and stopped via an `AddRollbackFunc` on disconnect/quit. Because `SocksListen`/`SocksEgress` travel in
the persisted `ConnectRequest` (`RequestRaw`), a daemon restart replays the connection and restarts the
proxy automatically. `kubevpn status -o wide` shows the listen address and mode (`egress`/`cluster`);
`-o json`/`-o yaml` expose `SocksListenAddr` / `SocksEgress`.

(This replaces the earlier model that forked a detached `proxy-out` subprocess tracked by a PID file.)
