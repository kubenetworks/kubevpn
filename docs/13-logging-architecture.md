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
│  Per-RPC logger (server-format, level = req.Level)            │
│       │                                                      │
│       ├──→ svr.LogFile (timestamp + [connID=… tun=…] + file:line) │
│       │    Debug only when req.Level=Debug (--debug)          │
│       │                                                      │
│       └──→ StreamHook ──→ gRPC stream (message only)         │
│            streamLevel = req.Level (Info default, Debug w/ --debug) │
│                                                              │
│  plog.G(context.Background()) ──→ global L ──→ logFile       │
│                                   (server-format, DebugLevel)│
└──────────────────────────────────────────────────────────────┘
```

### Key principle: one logger, two outputs, one level (the request's)

The daemon's per-RPC logger uses **server-format** as its primary formatter and its **level is
`req.Level`** — the CLI's `--debug` intent: **Info by default, Debug when the user passed
`--debug`**. Both outputs follow that level: the **log file** (`svr.LogFile`) and the **StreamHook**
(message-only → gRPC stream → CLI). So a non-`--debug` connection records no Debug lines at all
(file included) — which is what keeps per-packet tracing (gated by `IsDebugEnabled(ctx)`) from
flooding the file — while a `--debug` connection records Debug to both the file and the CLI. The
long-running background tasks a connect spawns (TUN, routes, DNS, per-packet) run on this logger's
context, so they inherit the connection's level too.

> This replaced an earlier "file is ALWAYS Debug" design: that made `IsDebugEnabled(ctx)` permanently
> true in the daemon and let per-packet logging flood the log file on every connection regardless of
> `--debug` (see docs/50). The global fallback `L` (used only by `context.Background()` logs) does
> still sit at Debug — see Rule 1.

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

### 3. File and stream both follow `req.Level`

The per-RPC logger's level IS `req.Level`, and both the file (`svr.LogFile`) and the StreamHook use it:
- no `--debug` → level `Info` → neither the file nor the CLI gets Debug lines (no `[Client-0] Connected`,
  `[Transport] Using TLS mode`, and — importantly — no per-packet lines to flood the file)
- `--debug` → level `Debug` → both the file and the CLI get Debug lines

The StreamHook's own `Level` is set to the same `req.Level` (redundant with the logger level, but
harmless): entries the logger admits are exactly those at/above `req.Level`, and the hook forwards
them to the CLI.

### 3a. Per-connection tagging (`connID`, `tun`) and `kubevpn logs` filtering

Connection-scoped handlers tag their context with the connection ID via
`plog.WithField(ctx, action.LogFieldConnID, id)`, and the data plane adds the TUN device name via
`plog.WithField(ctx, plog.FieldTun, name)` (core `TunHandler`). The server format renders them as a
`[connID=xxxx tun=utun5]` prefix (via `GenStr`), so concurrent operations sharing one daemon log
file can be filtered apart. The StreamHook uses the message-only client format, so the prefix never
reaches CLI stdout. core/gVISOR logs inherit the tags automatically through `plog.GetFields(ctx)`.

The root daemon tags `connID` from `req.ConnectionID` **as soon as the request arrives** (idempotency
guard, setup, data plane, and cleanup all carry it). The user daemon tags it only after computing the
ID from the namespace UID (needs the cluster client up), so its earliest connect lines are untagged;
`tun` exists only on the root daemon's data plane. A few one-time `context.Background()` setup logs
carry no tag at all.

`kubevpn logs --connection-id <id>` / `--tun <name>` filter on these tags with **lenient** semantics
(daemon side, `makeLogFilter`): a line is kept when it has no such tag (shared/early/setup logs, or
the user daemon which has no tun) **or** its tag matches; only a line tagged with a *different* value
is dropped. Tags match as whole tokens, so `connID=abc` doesn't match `connID=abcdef`. `--lines N`
still seeks the last N raw lines before filtering.

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
| Debug | ✅ only with `--debug` (per-RPC logger at req.Level) | ✅ only with `--debug` | ✅ (only with `--debug`) |
| Info | ✅ | ✅ | ✅ |
| Warn | ✅ | ✅ | ✅ |
| Error | ✅ | ✅ | ✅ |

Exception: logs emitted with `context.Background()` fall back to the global `L`, which the daemon
holds at Debug (Rule 1) — a small number of one-time setup lines that are not per-connection.

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
