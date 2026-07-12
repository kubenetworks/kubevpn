# SSH Architecture and Lifecycle

## Overview

The KubeVPN SSH subsystem provides two core capabilities:

1. **SSH Jump Host** — access Kubernetes clusters in private networks via SSH tunnels
2. **SSH Remote Terminal** — create interactive terminals on remote servers via WebSocket, optionally establishing a TUN tunnel

## Package Structure

```
pkg/ssh/                       SSH client core library
├── config.go                  SshConfig type + auth methods + ~/.ssh/config parsing
├── ssh.go                     Connection setup, port forwarding, kubeconfig tunnel
├── reverse.go                 Reverse tunnel (SSH -R equivalent)
├── scp.go                     SCP file transfer
├── gssapi.go                  GSSAPI/Kerberos authentication (SPNEGO negotiation)
├── gssapi_ccache.go           Kerberos credential cache parsing
├── gssapi_other.go            Kerberos config path (Unix)
├── gssapi_windows.go          Kerberos config path (Windows)
├── filename.go                IP → filename conversion utility
└── doc.go                     Package documentation

pkg/daemon/handler/            WebSocket SSH terminal service
├── ssh.go                     wsHandler — WebSocket entry point, terminal session
├── tunnel.go                  TUN tunnel setup, connWatcher
├── installer.go               Remote kubevpn installation
└── registry.go                SSH session registry (sync.Map)

pkg/daemon/action/
├── sshdaemon.go               SshStart/SshStop RPC — remote TUN server
├── sshconv.go                 RPC SshJump → SshConfig conversion
└── writer.go                  resolveKubeconfig — SSH tunnel kubeconfig resolution

cmd/kubevpn/cmds/
├── ssh.go                     `kubevpn ssh` command
└── sshdaemon.go               `kubevpn ssh-daemon` command (hidden, called on remote)
```

## Feature 1: SSH Jump Host

### Purpose

When the Kubernetes API Server is not directly reachable in a private network, access it via SSH tunnel:

```
┌──────────┐  SSH tunnel  ┌────────────┐       ┌──────────────────┐
│ Local PC  ├─────────────►│ Jump Host   ├──────►│ K8s API Server   │
└──────────┘ 127.0.0.1:N  └────────────┘       └──────────────────┘
```

### Core Flow

#### 1. Connection Setup — `DialSshRemote(ctx, conf, stopChan)`

`SshConfig` supports three entry methods, by priority:

| Field | Entry | Description |
|-------|-------|-------------|
| `ConfigAlias` | `AliasRecursion` | Parse alias from `~/.ssh/config`, recursively follow ProxyJump chain |
| `Jump` | `JumpRecursion` | Parse nested SSH jump config from `--ssh-jump` parameter |
| `Addr` | `Dial` | Direct connection to specified address |

**ProxyJump chain resolution** (`resolveProxyJumpChain`):

```
~/.ssh/config:
  Host A → ProxyJump B
  Host B → ProxyJump C
  Host C → (endpoint)

Resolution: [A, B, C]
Connection order: C → B → A (reverse, connect the farthest first)
```

- Uses a `visited` map to detect circular references, returning `"circular ProxyJump detected"` error when found
- Each jump host's auth info is read from `~/.ssh/config`, falling back to command-line defaults

#### 2. Authentication — `GetAuth()`

Tried in priority order:

| Priority | Method | Config Field |
|----------|--------|--------------|
| 1 | Password | `Password` |
| 2 | GSSAPI Password | `GSSAPIPassword` |
| 3 | GSSAPI Keytab | `GSSAPIKeytabConf` |
| 4 | GSSAPI Cache | `GSSAPICacheFile` |
| 5 | Public Key | `Keyfile` (default `~/.ssh/id_rsa`) |

GSSAPI authentication implements the full Kerberos 5 SPNEGO negotiation (`Krb5InitiatorClient`), including the state machine:
`InitiatorStart → InitiatorWaitForMutal → InitiatorReady`

#### 3. Kubeconfig Tunnel — `SshJump(ctx, conf, kubeconfigBytes, print)`

Full flow:

```
1. If RemoteKubeconfig is configured:
   a. SSH to remote server
   b. Execute kubectl/minikube/cat to get kubeconfig
   c. Replace local kubeconfig with the remote one

2. Pick an available local port N

3. Parse the API server address from kubeconfig (e.g., 10.0.1.100:6443)

4. Rewrite kubeconfig: API server → 127.0.0.1:N

5. Set up SSH port forwarding: 127.0.0.1:N → 10.0.1.100:6443
   (via PortMapUntil)

6. Write the rewritten kubeconfig to a temp file, return the path
   (temp file auto-deleted when ctx is cancelled)
```

#### 4. Port Forwarding — `PortMapUntil(ctx, conf, remote, local)`

Implements local TCP port forwarding:

- Listens on the `local` port locally
- Each inbound connection dials the `remote` address through an SSH channel
- Uses `sync.Map` to cache SSH client connections (`sshClientWrap`), auto-reconnects on failure
- Bidirectional data copy via `copyStream`, closes both ends when either direction completes or ctx is cancelled

#### 5. Daemon Integration

In each RPC action, the SSH jump host is integrated via `resolveKubeconfig`:

```go
// daemon/action/writer.go
func resolveKubeconfig(ctx, jump, kubeconfigBytes, portForward) (string, error) {
    sshConf := parseSshFromRPC(jump)  // RPC SshJump → SshConfig
    if !sshConf.IsEmpty() {
        return ssh.SshJump(ctx, sshConf, kubeconfigBytes, portForward)
    }
    return util.ConvertToTempKubeconfigFile(kubeconfigBytes, "")
}
```

**Callers:**

| RPC | File | Description |
|-----|------|-------------|
| Connect | `connect_elevate.go` | Control plane connection, also records `SshHosts` for route exclusion |
| Proxy | `proxy.go` | Proxy injection |
| Sync | `sync.go` | File synchronization |
| Reset | `reset.go` | Reset workloads |
| Disconnect | `disconnect.go` | Disconnect |
| Uninstall | `uninstall.go` | Uninstall |

**Connect special handling:**

```go
// connect_elevate.go — in user daemon
if sshConf := parseSshFromRPC(req.SshJump); !sshConf.IsEmpty() {
    connect.SshHosts = sshConf.Host()  // record jump host IP
    if sshConf.RemoteKubeconfig != "" {
        connect.OriginKubeconfigPath = file  // mark as using remote kubeconfig
    }
}
```

`SshHosts` is added to the `getAPIServerIPs()` return value, ensuring TUN routes do not override the route to the jump host (preventing SSH tunnel breakage).

### Lifecycle

```
[User executes kubevpn connect --ssh-addr ...]
    │
    ▼
parseSshFromRPC(req.SshJump) → SshConfig
    │
    ▼
resolveKubeconfig(ctx, jump, kubeconfigBytes, portForward)
    │
    ├── SshConfig.IsEmpty() == true → write temp kubeconfig directly
    │
    └── SshConfig.IsEmpty() == false:
        │
        ▼
    SshJump(ctx, conf, kubeconfigBytes, print)
        │
        ├── [optional] SSH remote kubeconfig retrieval
        │
        ├── Rewrite API server → 127.0.0.1:N
        │
        ├── PortMapUntil(ctx, conf, remote, local)
        │   │
        │   └── Background: listen → accept → SSH dial remote → bidirectional copy
        │
        └── Write temp kubeconfig file
            │
            └── Auto-deleted on ctx.Done()
```

**Teardown:** When the session context is cancelled (disconnect, daemon exit, user ctrl+c), all resources are cleaned up automatically:
- SSH client connection closed (via `sshClientWrap.Close()` calling cancel + client.Close)
- Local listening port closed
- Temp kubeconfig file deleted (`go func() { <-ctx.Done(); os.Remove(path) }()`)

## Feature 2: SSH Remote Terminal (`kubevpn ssh`)

### Purpose

Open an interactive terminal on a remote SSH server, optionally creating a bidirectional TUN tunnel for IP connectivity between local and remote machines.

### Architecture

```
┌────────────┐  WebSocket /ws   ┌──────────────┐    SSH     ┌─────────────┐
│ kubevpn ssh├─────────────────►│ Local daemon  ├──────────►│ Remote SSH  │
│ (CLI front)│                  │ (wsHandler)   │           │ (target)    │
│            │  WebSocket       │               │           │             │
│            │  /resize         │               │           │             │
└────────────┘                  └──────────────┘           └─────────────┘
```

### CLI Flow (`cmd/kubevpn/cmds/ssh.go`)

```
1. Check if stdin is a terminal
2. Get terminal width and height
3. Generate sessionID (UUID)
4. Build SSH config struct (Config, ExtraCIDR, Width, Height, Platform, SessionID, Lite)
5. JSON serialize to WebSocket header "ssh"
6. Connect to local daemon's Unix socket (GetTCPClient)
7. Create WebSocket connection to /ws
8. Start three concurrent goroutines:
   a. monitorSize — send terminal size changes via /resize WebSocket
   b. stdin → WebSocket (send user input)
   c. WebSocket → stdout (display remote output)
      with interleaved checker: detect "Enter terminal <sessionID>" marker
      after receiving marker, set terminal to raw mode
9. Wait for any goroutine to complete or ctx cancel
10. Restore terminal state
```

### Daemon Flow (`pkg/daemon/handler/`)

#### WebSocket Entry Point (`ssh.go` init registration)

**`/ws` endpoint:**

```go
http.Handle("/ws", websocket.Handler(func(conn) {
    // 1. Parse SSH config from header
    // 2. Register readiness signal: sessionRegistry.storeReady(sessionID, ctx)
    // 3. Create wsHandler
    // 4. Execute handle(lite)
    // 5. defer: sessionRegistry.cleanup(sessionID)
}))
```

**`/resize` endpoint:**

```go
http.Handle("/resize", websocket.Handler(func(conn) {
    // 1. Wait for session ready (via readyCtx.Done())
    // 2. Get SSH session from registry
    // 3. Loop reading TerminalSize JSON
    // 4. Call session.WindowChange(height, width)
}))
```

#### Session Registry (`registry.go`)

```go
var sessionRegistry = &registry{
    sessions: sync.Map{},  // map[sessionID] → *ssh.Session
    ready:    sync.Map{},  // map[sessionID] → context.Context
}
```

**Lifecycle:**

| Timing | Operation |
|--------|-----------|
| WebSocket connection established | `storeReady(id, ctx)` — register readiness context |
| SSH terminal ready | `storeSession(id, session)` — store SSH session; `condReady()` — cancel readiness context to notify /resize |
| WebSocket connection closed | `cleanup(id)` — delete sessions and ready entries |

#### wsHandler.handle(lite) Flow

```
handle(lite):
    │
    ├── DialSshRemote(ctx, sshConfig) → ssh.Client
    │
    ├── [lite == false] createTunnel(ctx, cli):
    │   │
    │   ├── installKubevpnOnRemote(ctx, cli)
    │   │   │
    │   │   ├── Check if remote already has kubevpn command
    │   │   ├── [not found] Download latest version → SCP to remote
    │   │   └── Start remote daemon (kubevpn status)
    │   │
    │   ├── Get local IP
    │   ├── PortMapUntil: 127.0.0.1:localPort → 127.0.0.1:10801 (remote)
    │   ├── RemoteRun: kubevpn ssh-daemon --client-ip <localIP>
    │   │   └── Remote daemon executes SshStart RPC
    │   │       ├── Create TUN device (net=198.18.0.0/32 route IP)
    │   │       ├── Start gvisor TCP listener (:10801)
    │   │       └── Add client IP route to TUN
    │   │
    │   ├── Create local TUN device + gvisor protocol stack
    │   │   └── Route: client IP → forward tcp://127.0.0.1:localPort
    │   │
    │   └── Start heartbeat ping (every 15 seconds)
    │
    ├── connWatcher wraps WebSocket (detect disconnect)
    │
    └── terminal(ctx, cli, rw):
        │
        ├── NewSession → bind stdin/stdout/stderr to WebSocket
        ├── sessionRegistry.storeSession(id, session)
        ├── condReady() — notify /resize that it can start
        ├── RequestPty("xterm-256color", height, width, modes)
        ├── Shell()
        └── session.Wait() — block until shell exits
```

### Two Modes

| Mode | `--lite` | Behavior |
|------|----------|----------|
| Full | false | Install kubevpn + create TUN tunnel + open terminal |
| Lite | true | Open terminal only (pure SSH terminal) |

**Full mode network topology:**

```
┌──────────┐  TUN   ┌──────────────┐  SSH port-forward  ┌──────────┐  TUN   ┌──────────┐
│ Local app ├───────►│ Local gvisor ├───────────────────►│ Remote   ├───────►│ Remote   │
│ 198.18.x │        │ (tcp forward)│                    │ gvisor   │        │ service  │
└──────────┘        └──────────────┘                    │ (:10801) │        │          │
                                                        └──────────┘        └──────────┘
```

### Connection Monitor — connWatcher

`connWatcher` wraps the WebSocket connection, notifying via a channel on any Read/Write error:

```go
type connWatcher struct {
    ch chan struct{}
    sync.Once  // ensure channel is only closed once
    net.Conn
}
```

When the WebSocket disconnects, the `closed()` channel is closed, triggering context cancel, which cascades cleanup across the entire session.

## Feature 3: SSH Remote TUN Server (`ssh-daemon`)

### RPC Definitions

```protobuf
rpc SshStart (SshStartRequest) returns (SshStartResponse) {}
rpc SshStop (SshStopRequest) returns (SshStopResponse) {}

message SshStartRequest { string ClientIP = 1; }
message SshStartResponse { string ServerIP = 1; }
message SshStopRequest { string ClientIP = 1; }
message SshStopResponse { string ServerIP = 1; }
```

### SshStart Logic (`daemon/action/sshdaemon.go`)

```
SshStart(ctx, req):
    │
    ├── Lock (mutual exclusion, prevent concurrent creation)
    │
    ├── Parse clientIP CIDR
    │
    ├── [sshServerIP == ""] First-time startup:
    │   │
    │   ├── Create TUN + gvisor nodes:
    │   │   ├── tun: net=198.18.0.0/32 (default server IP)
    │   │   └── gtcp: :10801 (gvisor TCP listener)
    │   │
    │   ├── Start handler.Run(sshCtx, servers)
    │   │
    │   └── Record sshServerIP, sshCancelFunc
    │
    ├── Find TUN device
    │
    ├── Add route: clientIP → TUN device
    │
    └── Return sshServerIP
```

### SshStop Logic

```go
func (svr *Server) SshStop(ctx, req) {
    if svr.sshCancelFunc != nil {
        svr.sshCancelFunc()  // close TUN + gvisor server
    }
}
```

### Server State Fields

```go
type Server struct {
    // ...
    sshServerIP   string             // current SSH TUN server IP
    sshCancelFunc context.CancelFunc // for shutting down SSH TUN service
}
```

**Multi-client support:** The TUN device is created only once (lazy initialization), subsequent clients only add routes. This means multiple `kubevpn ssh` clients can share the same TUN device.

## Lifecycle Summary

### SSH Jump Host Lifecycle

```
Creation:                                    Teardown:
  User RPC request                             session context cancelled
    → parseSshFromRPC                            → SSH client closed
    → resolveKubeconfig                          → local listening port closed
      → SshJump                                  → temp kubeconfig deleted
        → DialSshRemote (create SSH conn)
        → PortMapUntil (create local listener + port forwarding)
        → write temp kubeconfig

Lifetime = session context lifetime
```

### SSH Remote Terminal Lifecycle

```
Creation (Full mode):                        Teardown:
  CLI connects WebSocket /ws                   Triggered by any of:
    → DialSshRemote                            ├── WebSocket disconnect (connWatcher)
    → installKubevpnOnRemote                   ├── SSH shell exit (session.Wait)
    → Set up SSH port forwarding               ├── User ctrl+c (ctx cancel)
    → RemoteRun ssh-daemon                     └── daemon exit
      → remote SshStart RPC                  Cascade:
        → create TUN + gvisor                  → cancel ctx
    → Create local TUN + gvisor                → SSH session closed
    → Start heartbeat ping                     → SSH client closed
    → Open SSH terminal                        → local TUN stopped
    → Register in sessionRegistry              → sessionRegistry.cleanup
                                               → terminal state restored
                                             [remote TUN requires separate SshStop]
```

### SSH TUN Server Lifecycle

```
Creation:                                    Teardown:
  SshStart RPC                                 SshStop RPC
    → [lazy] create TUN + gvisor                 → sshCancelFunc()
    → add client route                           → TUN + gvisor closed

Lifetime: from first client SshStart to explicit SshStop
Note: sshCancelFunc is not automatically called on daemon exit,
      but process exit causes the TUN device to be reclaimed by the kernel.
```

## Proto Definitions

The `SshJump` message is embedded in the following RPC requests:

| Request Message | Field Number |
|-----------------|--------------|
| ConnectRequest | field 5 |
| DisconnectRequest | field 5 |
| ProxyRequest | field 9 |
| SyncRequest | field 8 |
| ResetRequest | field 4 |
| UninstallRequest | field 3 |

```protobuf
message SshJump {
  string Addr = 1;
  string User = 2;
  string Password = 3;
  string Keyfile = 4;
  string Jump = 5;           // nested jump config
  string ConfigAlias = 6;    // ~/.ssh/config alias
  string RemoteKubeconfig = 7;
  string GSSAPIKeytabConf = 8;
  string GSSAPIPassword = 9;
  string GSSAPICacheFile = 10;
}
```

## Related Design Documents

- `02-dual-daemon.md` — Dual daemon model (resolveKubeconfig runs in user daemon)
- `12-session-lifecycle.md` — SessionLifecycle (manages SSH tunnel temp file cleanup)
- `14-rpc-daemon-mapping.md` — RPC to daemon mapping
