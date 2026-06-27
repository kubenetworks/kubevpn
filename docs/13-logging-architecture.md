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
│  Per-RPC logger (server-format, DebugLevel)                   │
│       │                                                      │
│       ├──→ svr.LogFile (timestamp + file:line + level)       │
│       │    ALL levels including Debug                         │
│       │                                                      │
│       └──→ StreamHook ──→ gRPC stream (message only)         │
│            Info+ only (Debug filtered out)                    │
│                                                              │
│  plog.G(context.Background()) ──→ global L ──→ logFile       │
│                                   (server-format, DebugLevel)│
└──────────────────────────────────────────────────────────────┘
```

### Key principle: one logger, two outputs, two formats, two levels

The daemon's per-RPC logger uses **server-format** as its primary formatter (writes to log file at DebugLevel with full debug info). A **StreamHook** intercepts log entries at **InfoLevel** and sends only the message text to the gRPC stream. Debug messages go to the log file only.

```go
// Daemon per-RPC setup (writer.go initStreamLogger):
logger := plog.GetLoggerForServer(level, svr.LogFile)  // server-format → log file (all levels)
logger.AddHook(&plog.StreamHook{                        // message-only → gRPC stream
    Writer: newStreamWriter(sendMsg),
    Level:  log.InfoLevel,                               // Info+ only — Debug stays in log file
})
ctx = plog.WithLogger(resp.Context(), logger)
```

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

### 3. StreamHook filters debug from CLI

StreamHook level is always `InfoLevel`, regardless of the logger's own level. This means:
- `--debug` flag → daemon logger at `DebugLevel` → debug messages in log file
- StreamHook → only `Info`/`Warn`/`Error` sent to CLI via gRPC stream
- User never sees `[Client-0] Connected`, `[Transport] Using TLS mode`, etc.

### 4. `plog.G(context.Background())` fallback

Code using `context.Background()` falls back to global `L`:
- In CLI process: `L` is `InfoLevel` → debug messages suppressed, errors go to stderr
- In daemon process: `L` is `DebugLevel` after startup → all messages go to log file

### 5. Log levels

| Level | Log file | gRPC stream → CLI | CLI stdout |
|---|---|---|---|
| Debug | ✅ (daemon only, after L upgrade) | ❌ (StreamHook filters) | ✅ (only with `--debug`) |
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
| `GetLoggerForClient(level, out)` | `pkg/log/logger.go` | Create client-format logger for custom output |
| `GetLoggerForServer(level, out)` | `pkg/log/logger.go` | Create server-format logger (timestamp+file:line) |
| `StreamHook` | `pkg/log/logger.go` | Logrus hook: sends message-only text to a writer at InfoLevel |
| `initStreamLogger` | `pkg/daemon/action/writer.go` | Create per-RPC logger: server-format to logFile + StreamHook to gRPC |
| `serverFormat` | `pkg/log/logger.go` | `2006-01-02 15:04:05.000 file.go:42 level: message` |
| `format` (client) | `pkg/log/logger.go` | `message\n` |

## Log Output Examples

**CLI stdout** (`kubevpn connect`):
```
Starting connect to cluster
Forwarding port...
Allocated TUN IP: v4=198.18.0.5/16 v6=2001:2::5/64
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
2026-06-10 08:15:25.234 network.go:204 info: Allocated TUN IP: v4=198.18.0.5/16 v6=2001:2::5/64
2026-06-10 08:15:25.345 tun_server.go:92 warning: [Perf] Slow tunInbound send blocked 25ms
2026-06-10 08:15:26.456 network.go:142 info: Adding Pod IP and Service IP to route table...
```

Note: debug lines (`[Gvisor-TCP]`, `[Transport]`, `[Client-0]`) appear only in the log file, never in CLI output.
