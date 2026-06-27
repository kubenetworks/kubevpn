# Logging Architecture

## Overview

KubeVPN runs as three processes: **CLI** (user-facing command), **User Daemon** (unprivileged, control plane), and **Root Daemon** (privileged, data plane). Each has different logging requirements:

| Process | Output | Format | Destination |
|---|---|---|---|
| CLI | Simple progress messages | `message\n` | stdout |
| User/Root Daemon | Full structured log | `2006-01-02 15:04:05.000 file.go:42 info: message` | log file (lumberjack) |
| Daemon → CLI | Progress streamed to user | `message\n` | gRPC stream → CLI stdout |

## Current Problems

### 1. Mutable global logger `L`

```go
// pkg/log/context.go
var L = InitLoggerForServer()  // server-format at package init time
```

Every CLI command overwrites `L` in `PreRunE`:

```go
plog.InitLoggerForClient()  // MUTATES global L to client-format
```

This causes:
- Before `PreRunE`: CLI log uses server-format (user sees `file.go:42 info: message`)
- After `PreRunE`: L is client-format, but daemon goroutines spawned later also see client-format L
- Race condition: multiple goroutines read L while CLI mutates it

### 2. Daemon log file gets client-format messages

```go
// pkg/daemon/action/writer.go
logger := plog.GetLoggerForClient(level, io.MultiWriter(
    newStreamWriter(sendMsg),  // gRPC stream → CLI (OK: simple messages)
    svr.LogFile,               // log file (BAD: same simple format, no timestamp/file:line)
))
```

One logger, one formatter, two outputs. The log file gets `message\n` without any debugging context.

### 3. `plog.G(context.Background())` fallback

31 call sites in `pkg/` use `plog.G(context.Background())`. These fall back to global `L`, whose format depends on which process mutated it last.

## Target Architecture

```
┌─────────────────────────────────────────────────────────────┐
│ CLI Process                                                  │
│                                                              │
│  cmd.SetContext(WithLogger(ctx, clientLogger))               │
│       │                                                      │
│       ▼                                                      │
│  plog.G(ctx) ──→ clientLogger ──→ stdout (message only)      │
│                                                              │
│  gRPC stream recv ──→ print to stdout (message only)         │
└─────────────────────────────────────────────────────────────┘
                          ▲ gRPC stream
                          │
┌─────────────────────────┼───────────────────────────────────┐
│ Daemon Process           │                                    │
│                          │                                    │
│  Per-RPC logger (server-format)                               │
│       │                                                      │
│       ├──→ svr.LogFile (timestamp + file:line + level)       │
│       │                                                      │
│       └──→ StreamHook ──→ gRPC stream (message only)         │
│                                                              │
│  plog.G(context.Background()) ──→ global L ──→ logFile       │
│                                   (server-format, fallback)  │
└──────────────────────────────────────────────────────────────┘
```

### Key principle: one logger, two outputs, two formats

The daemon's per-RPC logger uses **server-format** as its primary formatter (writes to log file with full debug info). A **StreamHook** intercepts each log entry and sends only the message text to the gRPC stream.

```go
// Daemon per-RPC setup:
logger := plog.GetLoggerForServer(level, svr.LogFile)  // server-format → log file
logger.AddHook(&plog.StreamHook{                        // message-only → gRPC stream
    Writer: newStreamWriter(sendMsg),
    Level:  log.Level(level),
})
ctx = plog.WithLogger(resp.Context(), logger)
```

## Design Rules

### 1. Never mutate global `L`

`L` is initialized once as server-format and never reassigned. `InitLoggerForClient()` (which overwrites `L`) should be removed. CLI injects its logger via context:

```go
// Before (bad): mutates global state
plog.InitLoggerForClient()

// After (good): context-scoped, no global mutation
cmd.SetContext(plog.WithLogger(cmd.Context(), plog.GetLoggerForClient(...)))
```

### 2. CLI logger lives in `cmd.Context()`

CLI commands create a client-format logger and inject it into `cmd.Context()`. All CLI-side code uses `plog.G(cmd.Context())` or `plog.G(ctx)` — never `plog.G(context.Background())`.

### 3. Daemon logger uses StreamHook for dual output

Daemon RPC handlers create a server-format logger writing to the log file, with a StreamHook that sends message-only text to the gRPC stream. This gives:
- Log file: `2006-01-02 15:04:05.000 connect.go:42 info: Connecting to cluster`
- gRPC stream → CLI: `Connecting to cluster`

### 4. `plog.G(context.Background())` is always server-format

Any code that uses `context.Background()` falls back to global `L` (server-format). In daemon, this writes to the log file. In CLI, this writes to stderr. Both are acceptable fallback behaviors.

### 5. Log levels flow from CLI to daemon

CLI passes the desired log level via `ConnectRequest.Level`. The daemon creates its per-RPC logger at that level. Debug-level messages appear in the log file but are filtered from the gRPC stream (StreamHook respects the level).

## Component Reference

| Component | File | Purpose |
|---|---|---|
| `L` (global) | `pkg/log/context.go` | Immutable server-format fallback logger |
| `G(ctx)` | `pkg/log/context.go` | Get logger from context, fallback to `L` |
| `WithLogger(ctx, logger)` | `pkg/log/context.go` | Inject logger into context |
| `GetLoggerForClient` | `pkg/log/logger.go` | Create client-format logger (message-only) |
| `GetLoggerForServer` | `pkg/log/logger.go` | Create server-format logger (timestamp+file:line) |
| `StreamHook` | `pkg/log/logger.go` | Hook that sends message-only text to a writer |
| `initStreamLogger` | `pkg/daemon/action/writer.go` | Create per-RPC logger with dual output |
| `serverFormat` | `pkg/log/logger.go` | `2006-01-02 15:04:05.000 file.go:42 level: message` |
| `format` (client) | `pkg/log/logger.go` | `message\n` |

## Log Output Examples

**CLI stdout** (what user sees when running `kubevpn connect`):
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
2026-06-10 08:15:24.123 network.go:122 info: Forwarding port...
2026-06-10 08:15:25.234 network.go:204 info: Allocated TUN IP: v4=198.18.0.5/16 v6=2001:2::5/64
2026-06-10 08:15:25.345 tun_server.go:92 warning: [Perf] Slow tunInbound send: 10.0.0.1 -> 10.0.0.5 blocked 25ms
2026-06-10 08:15:26.456 network.go:142 info: Adding Pod IP and Service IP to route table...
2026-06-10 08:15:27.567 network.go:148 info: Configuring DNS service...
```

**Fallback** (pkg/ code using `context.Background()` in daemon):
```
2026-06-10 08:15:23.100 gvisor_tcp_handler.go:73 info: [Gvisor-TCP] Listening on :10801
2026-06-10 08:15:23.101 gvisor_udp_handler.go:54 info: [UDP] Listening on :10802
```
