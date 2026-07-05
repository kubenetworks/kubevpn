# Logging Architecture

## Overview

KubeVPN runs as three processes: **CLI** (user-facing command), **User Daemon** (unprivileged, control plane), and **Root Daemon** (privileged, data plane). Each has different logging requirements:

| Process | Output | Format | Destination |
|---|---|---|---|
| CLI | Simple progress messages | `message\n` | stdout |
| User/Root Daemon | Full structured log | `2006-01-02 15:04:05.000 file.go:42 info: message` | log file (lumberjack) |
| Daemon → CLI | Progress streamed to user | `message\n` | gRPC stream → CLI stdout |

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│ CLI Process                                                  │
│                                                              │
│  PreRunE: cmd.SetContext(WithLogger(ctx, NewClientLogger())) │
│       │                                                      │
│       ▼                                                      │
│  plog.G(ctx) ──→ clientLogger ──→ stdout (message only)      │
│                                                              │
│  gRPC stream recv ──→ print to stdout (message only)         │
│                                                              │
│  plog.G(context.Background()) ──→ global L ──→ stderr        │
│                                   (server-format, InfoLevel) │
└─────────────────────────────────────────────────────────────┘
                          ▲ gRPC stream (Info+ only)
                          │
┌─────────────────────────┼───────────────────────────────────┐
│ Daemon Process           │                                    │
│                          │                                    │
│  Per-RPC logger (server-format, file ALWAYS DebugLevel)       │
│       │                                                      │
│       ├──→ svr.LogFile (timestamp + [connID=…] + file:line)  │
│       │    ALL levels including Debug (always)                │
│       │                                                      │
│       └──→ StreamHook ──→ gRPC stream (message only)         │
│            streamLevel = req.Level (Info default, Debug w/ --debug) │
│                                                              │
│  plog.G(context.Background()) ──→ global L ──→ logFile       │
│                                   (server-format, DebugLevel)│
└──────────────────────────────────────────────────────────────┘
```

### Key principle: one logger, two outputs, two formats, two levels

The daemon's per-RPC logger uses **server-format** as its primary formatter and **always writes
to the log file at DebugLevel** — independent of the CLI `--debug` flag, so the file is always a
full record for post-mortem debugging. A **StreamHook** sends only the message text to the gRPC
stream at **`streamLevel`**, which is `req.Level` (the CLI's `--debug` intent): **Info by default,
Debug when the user passed `--debug`**. So Debug lines always land in the file, and additionally
reach the CLI only when `--debug` is set.

The same StreamHook also carries the connect progress-step markers (used to drive the CLI spinner)
without polluting the log file — see [30-connect-progress.md](30-connect-progress.md).

```go
// Daemon per-RPC setup (writer.go newServerStreamLogger / initStreamLogger):
logger := plog.GetLoggerForServer(int32(log.DebugLevel), svr.LogFile) // file: ALL levels, always
logger.AddHook(&plog.StreamHook{                                      // message-only → gRPC stream
    Writer: newStreamWriter(sendMsg),
    Level:  log.Level(streamLevel),  // req.Level: Info default, Debug with --debug
})
ctx = plog.WithLogger(resp.Context(), logger)
```

Connect/Proxy/Sync/Disconnect/Quit/Leave/Reset/Unsync/Uninstall all carry `Level` (populated by `plog.GetLogLevel()` in the CLI command handler) so the daemon's StreamHook forwards logs at the user-requested level. The zero-value guard in `newServerStreamLogger` treats a missing Level as Info (e.g. from an older client).

## Design Rules

### 1. Global `L` is immutable

`L` is initialized once as server-format at `InfoLevel`. The daemon upgrades it to `DebugLevel` after redirecting output to the log file (`daemon.go`). CLI never mutates `L`.

```go
// pkg/log/context.go — initialized once at package init
var L = InitLoggerForServer()  // server-format, InfoLevel, stderr

// pkg/daemon/daemon.go — daemon upgrades after output redirect
plog.L.SetOutput(l)         // redirect to lumberjack log file
plog.L.SetLevel(log.DebugLevel)  // enable debug in log file
```

### 2. CLI logger lives in `cmd.Context()`

CLI commands create a client-format logger and inject it into `cmd.Context()`:

```go
// Every CLI command's PreRunE:
cmd.SetContext(plog.WithLogger(cmd.Context(), plog.NewClientLogger()))
```

`NewClientLogger()` returns a message-only logger writing to stdout. Respects `--debug` flag via `config.Debug`.

### 3. StreamHook level follows `--debug`; the file is always Debug

The file side is **always** `DebugLevel`. The StreamHook level is `req.Level`:
- no `--debug` → StreamHook at `Info` → CLI sees only `Info`/`Warn`/`Error`; Debug stays in the file
  (user never sees `[Client-0] Connected`, `[Transport] Using TLS mode`, etc.)
- `--debug` → StreamHook at `Debug` → CLI also sees Debug lines (file unchanged, always Debug)

### 3a. Per-connection tagging (`connID`)

Connection-scoped handlers tag their context with the connection ID via
`plog.WithField(ctx, action.LogFieldConnID, id)`. The server format renders it as a `[connID=xxxx]`
prefix (via `GenStr`), so concurrent operations sharing one daemon log file can be filtered apart
(`grep connID=xxxx`). The StreamHook uses the message-only client format, so the prefix never reaches
CLI stdout. Tagged at: connect (root + user daemon, on `session.Ctx`), proxy, and disconnect-by-id.
core/gVISOR logs inherit the tag automatically through `plog.GetFields(ctx)`.

### 3b. In-cluster sidecars default to Debug

The traffic-manager `server` and `control-plane` containers, and the injected fargate `server`
sidecar, are deployed with `--debug` (`pkg/handler/traffmgr_resources.go`, `pkg/inject/container.go`)
so `kubectl logs` shows Debug by default. `control-plane` applies `config.Debug` to its logger in
`controlplane.Main`. The `dns` container has no debug flag and is left at Info.

### 4. `plog.G(context.Background())` fallback

Code using `context.Background()` falls back to global `L`:
- In CLI process: `L` is `InfoLevel` → debug messages suppressed, errors go to stderr
- In daemon process: `L` is `DebugLevel` after startup → all messages go to log file

### 5. Log levels

| Level | Log file | gRPC stream → CLI | CLI stdout |
|---|---|---|---|
| Debug | ✅ (always, both daemons) | ✅ only with `--debug` (StreamHook at req.Level) | ✅ (only with `--debug`) |
| Info | ✅ | ✅ | ✅ |
| Warn | ✅ | ✅ | ✅ |
| Error | ✅ | ✅ | ✅ |

## Component Reference

| Component | File | Purpose |
|---|---|---|
| `L` (global) | `pkg/log/context.go` | Immutable server-format fallback logger (InfoLevel default, DebugLevel in daemon) |
| `G(ctx)` | `pkg/log/context.go` | Get logger from context, fallback to `L` |
| `WithLogger(ctx, logger)` | `pkg/log/context.go` | Inject logger into context |
| `NewClientLogger()` | `pkg/log/logger.go` | Create client-format logger for CLI (message-only, stdout) |
| `GetLogLevel()` | `pkg/log/logger.go` | Return DebugLevel or InfoLevel based on `config.Debug`; CLI commands use it to populate RPC `Level` |
| `IsDebugEnabled(ctx)` | `pkg/log/context.go` | Guard expensive debug-only work (per-packet parsing) without relying on the global `config.Debug` flag |
| `GetLoggerForClient(level, out)` | `pkg/log/logger.go` | Create client-format logger for custom output |
| `GetLoggerForServer(level, out)` | `pkg/log/logger.go` | Create server-format logger (timestamp+file:line) |
| `StreamHook` | `pkg/log/logger.go` | Logrus hook: sends message-only text to a writer at its configured Level |
| `newServerStreamLogger` | `pkg/daemon/action/writer.go` | Build per-RPC logger: file always Debug + StreamHook at streamLevel (req.Level) |
| `initStreamLogger` | `pkg/daemon/action/writer.go` | `newServerStreamLogger` + `WithLogger(resp.Context())` |
| `LogFieldConnID` | `pkg/daemon/action/writer.go` | ctx field key `"connID"` → `[connID=xxxx]` prefix for per-connection isolation |
| `serverFormat` | `pkg/log/logger.go` | `2006-01-02 15:04:05.000 file.go:42 level: message` |
| `format` (client) | `pkg/log/logger.go` | `message\n` |

## Log Output Examples

**CLI stdout** (`kubevpn connect`):
```
Starting connect to cluster
Forwarding port...
Allocated TUN IP: v4=198.18.0.5/32 v6=2001:2::5/128
Adding Pod IP and Service IP to route table...
Configuring DNS service...
Now you can access resources in the kubernetes cluster !
```

**Daemon log file** (`~/.kubevpn/daemon/daemon.log`):
```
2026-06-10 08:15:23.456 connect_elevate.go:89 info: Use manager namespace default
2026-06-10 08:15:23.567 connect.go:143 info: Starting connect to cluster
2026-06-10 08:15:23.600 gvisor_tcp_handler.go:73 debug: [Gvisor-TCP] Listening on :10801
2026-06-10 08:15:24.123 network.go:122 info: Forwarding port...
2026-06-10 08:15:24.200 transporter_tcp.go:29 debug: [Transport] Using TLS mode
2026-06-10 08:15:24.300 tun_client.go:126 debug: [Client-0] Connected to 127.0.0.1:51496
2026-06-10 08:15:24.310 tun_client.go:263 debug: [Client-0] OUTBOUND SRC: 198.18.0.5, DST: 10.0.0.5, Protocol: TCP, Length: 60
2026-06-10 08:15:24.320 tun_client.go:198 debug: [Client-0] INBOUND SRC: 10.0.0.5, DST: 198.18.0.5, Protocol: TCP, Length: 52
2026-06-10 08:15:25.234 network.go:204 info: Allocated TUN IP: v4=198.18.0.5/32 v6=2001:2::5/128
2026-06-10 08:15:25.345 tun_server.go:92 warning: [Perf] Slow tunInbound send blocked 25ms
2026-06-10 08:15:26.456 network.go:142 info: Adding Pod IP and Service IP to route table...
```

Note: debug lines (`[Gvisor-TCP]`, `[Transport]`, `[Client-0]`) always go to the log file; they reach
CLI stdout only when the user passed `--debug`. With multiple concurrent operations, each line in the
file carries a `[connID=xxxx]` prefix so they can be filtered apart.

At Debug, the client logs **every** packet on both directions of the data path: `OUTBOUND` in the
client read-tun path (`clientTransport.routeOutbound`, local app → cluster) and `INBOUND` in the
per-connection reader (`connSlot.readFromConn`, cluster → local app), each with src/dst/protocol/length.
