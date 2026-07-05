# Daemon Bootstrap & IPC Lifecycle

## 1. Overview

KubeVPN runs two long-lived background daemons — an unprivileged **user daemon** and a
privileged **root (sudo) daemon** — that the CLI talks to over local Unix-domain-socket gRPC.
This document covers the *mechanics* of those processes: how a daemon binds its socket, writes
its PID file, serves gRPC (with HTTP downgrade), detects its own staleness, and how the client
side caches connections, negotiates versions, and spawns daemons on demand.

The *responsibility split* (which RPC runs where) is in
[02-dual-daemon.md](02-dual-daemon.md) and [14-rpc-daemon-mapping.md](14-rpc-daemon-mapping.md);
this doc is the process/transport layer beneath them.

Server side: `SvrOption` (`pkg/daemon/daemon.go`), launched by `CmdDaemon`
(`cmd/kubevpn/cmds/daemon.go`). Client side: `GetClient` / `StartupDaemon` / `runDaemon`
(`pkg/daemon/client.go`).

## 2. Two Processes, Fixed Paths

Each daemon owns a fixed triple of files under the kubevpn config dir, keyed by privilege:

| | User daemon | Root daemon |
|---|---|---|
| Socket | `user_daemon.sock` | `root_daemon.sock` |
| PID file | `user_daemon.pid` | `root_daemon.pid` |
| Log file | `user_daemon.log` | `root_daemon.log` |
| pprof port | `config.PProfPort` | `config.SudoPProfPort` |

Resolved via `config.GetSockPath/GetPidPath/GetDaemonLogPath(isSudo)`. The `--sudo` flag on
`kubevpn daemon` selects which side this process is.

## 3. Server Startup (`SvrOption.Start`)

```
Start(ctx)
  ├── initLogging(isSudo)              ── lumberjack rotating file; wire logrus/klog/gvisor/plog
  ├── remove stale socket; net.Listen("unix", sockPath)
  │     └── SetUnlinkOnClose(false)   ── do NOT unlink on close (stale-detection owns the file)
  ├── go detectUnixSocksFile(ctx)     ── self-staleness watchdog (§5)
  ├── chmod socket 0666 (FileModeSocket)   ── any local user can reach the user daemon
  ├── writePIDToFile(isSudo)          ── os.Getpid() → pid file (FileModeFile 0644)
  ├── grpc.NewServer(
  │       ChainUnary : UnaryPanicHandler → UnaryClassifyInterceptor
  │       ChainStream: StreamPanicHandler → StreamClassifyInterceptor)
  ├── admin.Register + health server + reflection.Register
  ├── h2c downgrading http.Server (§4)
  ├── svr = action.Server{Cancel: cleanup, IsSudo, GetClient, LogFile, ID}
  │     └── if !IsSudo: go svr.LoadFromConfig(ctx)   ── replay persisted connections
  ├── RegisterDaemonServer(grpc, svr)
  └── downgradingServer.Serve(lis)    ── blocks until ctx cancel / Close
```

The interceptor order matters: the **panic handler is outermost** (turns panics into
`codes.Internal`), then the **classify interceptor** maps remaining handler errors to gRPC status
codes carrying exit-code detail — see [31-exit-codes.md](31-exit-codes.md) and
[13-logging-architecture.md](13-logging-architecture.md).

`cleanup` (passed as `action.Server.Cancel`) quits the server, closes the HTTP server (required
or the daemon will not exit), and closes the log file.

## 4. Single Port, gRPC + HTTP (`createDowngradingHandler` / h2c)

The daemon serves both gRPC and plain HTTP on the one Unix socket. `createDowngradingHandler`
routes by request shape:

- `ProtoMajor == 2` **and** `Content-Type: application/grpc` → the gRPC server.
- everything else → `http.DefaultServeMux` (used by the SSH WebSocket terminal handlers in
  `pkg/daemon/handler`, registered via blank import — see [15-ssh-architecture.md](15-ssh-architecture.md)).

It is wrapped in `h2c.NewHandler` so HTTP/2 works **without TLS** (cleartext) over the local
socket. `GetTCPClient` returns a raw `net.Conn` to the same socket for the HTTP/WebSocket path.

## 5. Self-Staleness Watchdog (`detectUnixSocksFile`)

A daemon stops itself if it becomes stale. Two triggers run concurrently:

1. **fsnotify** watcher on the PID file — reacts to changes immediately.
2. **2-second poll** loop — a fallback heartbeat.

Both run the same check `f()`:
- socket file gone (`os.ErrNotExist`) ⇒ `Stop()`.
- otherwise dial the socket (uncached) and call `Identify`; if the returned `ID` differs from this
  process's random `ID` ⇒ a newer daemon took over ⇒ `Stop()`.

The `ID` is a 32-byte random base64 string generated per process in `CmdDaemon.PreRunE`. This is
why `SetUnlinkOnClose(false)` is used — the file's presence/identity is a liveness signal, not just
a socket artifact.

## 6. Client Side (`GetClient`)

`GetClient(isSudo)` returns a **cached** `rpc.DaemonClient` (`daemonClient` / `sudoDaemonClient`
guarded by `clientMu`):

1. If the socket file is missing ⇒ `ErrDaemonNotRunning`.
2. Return the cached client if present.
3. Otherwise `grpc.NewClient("unix:"+sock, insecure, WithNoProxy)`.
4. **Health gate**: gRPC health `Check`; non-`SERVING` ⇒ `ErrDaemonNotRunning`.
5. **Version negotiation**: call `Upgrade{ClientVersion}`; if `NeedUpgrade`, send `Quit` to the
   outdated daemon, **do not cache**, and return `ErrDaemonVersionMismatch` so the caller spawns a
   fresh daemon.
6. Cache and return.

`getClientWithoutCache` is the uncached variant used by the watchdog (it must not poison the
cache).

## 7. Spawning Daemons (`StartupDaemon` / `runDaemon`)

`StartupDaemon` ensures **both** daemons exist: it `GetClient(false)` then `GetClient(true)`,
calling `runDaemon` for whichever is absent.

`runDaemon(exe, isSudo)`:
1. Short-circuit if `GetClient` already succeeds.
2. Remove any stale PID file.
3. Launch via `pkg/daemon/elevate` (see [34-privilege-escalation.md](34-privilege-escalation.md)):
   - sudo + not admin → `RunCmdWithElevated(exe, ["daemon","--sudo"])` (UAC/sudo prompt)
   - sudo + admin → `RunCmd(exe, ["daemon","--sudo"])`
   - user → `RunCmd(exe, ["daemon"])`
4. Poll for the PID file every **50ms** until it appears, then re-acquire the client.

## 8. Logging & pprof

`initLogging` builds a **lumberjack** rotating logger (`100MB` max size, `3` backups, `3` days,
local time, daily `Rotate()` at midnight via `rotateLog`) and redirects logrus, std `log`, klog,
gvisor `glog`, and `plog.L` into it at Debug level; `rest` client warnings are silenced.
`CmdDaemon` additionally starts a pprof server per side and, for the sudo daemon, dumps all pprof
profiles to disk on exit.

## 9. Related Files

| File | Purpose |
|---|---|
| `pkg/daemon/daemon.go` | `SvrOption.Start/Stop`, h2c handler, watchdog, logging/rotation |
| `pkg/daemon/client.go` | `GetClient`, `StartupDaemon`, `runDaemon`, `GetTCPClient` |
| `cmd/kubevpn/cmds/daemon.go` | `CmdDaemon`, per-process random `ID`, pprof, `--sudo` flag |
| `pkg/config/const.go` | sock/pid/log path constants, `FileModeSocket`, `FileModeFile` |
| `pkg/daemon/rpc/classifyinterceptor.go` | unary/stream classify + panic interceptors |

## 10. Related Docs

- [02-dual-daemon.md](02-dual-daemon.md) — control-plane vs data-plane responsibility split
- [14-rpc-daemon-mapping.md](14-rpc-daemon-mapping.md) — which RPC runs in which daemon
- [34-privilege-escalation.md](34-privilege-escalation.md) — how daemons get launched/elevated
- [13-logging-architecture.md](13-logging-architecture.md) — log routing through the daemons
- [12-session-lifecycle.md](12-session-lifecycle.md) — per-RPC session cleanup atop this server
