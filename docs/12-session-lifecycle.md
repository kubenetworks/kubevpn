# Context Lifecycle & SessionLifecycle Design

## Overview

KubeVPN daemon actions (Connect, Sync) use `context.Context` to manage the lifecycle of SSH tunnels, VPN connections, and cleanup operations. The `SessionLifecycle` struct centralizes context + cleanup management, replacing the previous ad-hoc pattern of scattered `ctx/cancel/defer` calls.

## Why Not `resp.Context()`?

Daemon sessions are **long-lived state** that outlive the initial gRPC call:

```go
// WRONG — kills the VPN when the CLI process exits:
ctx, cancel := context.WithCancel(resp.Context())

// CORRECT — VPN survives CLI disconnect:
session := NewSessionLifecycle(logger)  // uses context.Background()
```

The user runs `kubevpn connect`, the CLI prints "Connected" and exits. The tunnel stays alive. `resp.Context()` would kill it on exit. Instead, `ListenCancel(resp, session.Cancel)` provides **opt-in cancellation** — the client can send a Cancel message to explicitly tear down, but a normal exit doesn't.

## SessionLifecycle

```go
// pkg/daemon/action/lifecycle.go

type SessionLifecycle struct {
    Ctx    context.Context      // session lifecycle — cancel to tear down everything
    Cancel context.CancelFunc   // explicit cancel (e.g., duplicate connection, client cancel)
    // + mutex-protected cleanup list, idempotent RunCleanups
}
```

### API

| Method | Purpose |
|--------|---------|
| `NewSessionLifecycle(logger)` | Creates session with `context.Background()` + attached logger |
| `session.Ctx` | Use as the parent context for all session operations |
| `session.Cancel` | Cancel the context (triggers SSH tunnel close, port-forward stop, etc.) |
| `session.AddCleanup(func)` | Register a cleanup function (runs in LIFO order) |
| `session.AddTempFile(&path)` | Convenience: register temp file for removal |
| `session.RunCleanups()` | Execute all cleanups (reverse order) + Cancel + detach logger. **Idempotent.** |

### Properties

- **LIFO cleanup order**: Last registered = first executed (like `defer`)
- **Idempotent**: `RunCleanups()` can be called multiple times safely (mutex + `done` flag)
- **Thread-safe**: `AddCleanup` and `RunCleanups` are synchronized via mutex
- **Automatic logger detach**: `plog.WithoutLogger()` called automatically in `RunCleanups()`

## The Three Session Scopes

### 1. Connect — Sudo Daemon (Data Plane)

No SSH tunnel. Direct kubeconfig file.

```
session := NewSessionLifecycle(logger)

session.AddTempFile(&file)                    ← remove kubeconfig on cleanup
ds.AddRollbackFunc(session.RunCleanups)       ← cleanup on disconnect
go grpcutil.ListenCancel(resp, session.Cancel)    ← client cancel → session cancel

ds.DoConnect(session.Ctx)                     ← DataSession, root daemon
    └── ds.ctx = WithCancel(session.Ctx)      ← internal lifecycle
           ├── portForward(ds.ctx)
           ├── startLocalTunServer(ds.ctx)
           ├── addRouteDynamic(ds.ctx)
           └── setupDNS(ds.ctx)

Teardown: session.Cancel() → ds.ctx cancelled → TUN/DNS/routes stop
```

### 2. Connect — User Daemon (Control Plane + SSH)

SSH tunnel to bastion host, then forwards to sudo daemon.

```
session := NewSessionLifecycle(logger)

session.AddTempFile(&file)                         ← remove kubeconfig
connect.AddRollbackFunc(session.RunCleanups)       ← cleanup on disconnect

resolveKubeconfig(session.Ctx, SshJump, ...)       ← SSH tunnel created
    └── ssh.SshJump(session.Ctx, ...)
           └── PortMapUntil(session.Ctx, ...)
                  └── go func() { <-session.Ctx.Done(); close tunnel }

detectAndSetManagerNamespace(session.Ctx, ...)
connect.InitDHCP(session.Ctx)
forwardConnectToSudo(session.Ctx, ...)
    ├── req.OwnerID = connect.OwnerID
    └── cli.Connect(ctx)                           ← to sudo daemon

Teardown: session.Cancel() → SSH tunnel closes → temp kubeconfig removed
```

### 3. Sync

SSH tunnel for kubeconfig, independent VPN connection.

```
session := NewSessionLifecycle(logger)

session.AddTempFile(&file)
options.AddRollbackFunc(session.RunCleanups)

cli.Connect(context.Background())                  ← independent! survives sync
resolveKubeconfig(session.Ctx, ...)                ← SSH tunnel
options.SetContext(session.Ctx)
options.DoSync(session.Ctx, ...)

Teardown (on error): session.Cancel() → SSH closes, BUT VPN stays alive
    (because cli.Connect used Background, not session.Ctx)
```

**Key design**: `cli.Connect(context.Background())` is intentional — the VPN connection must survive sync failure so the user can retry without reconnecting.

## Cleanup Two-Path Design

Cleanup dispatches by type identity — no flag or nil-check needed:

- **`ConnectOptions.Cleanup()`** is always the control-plane path (user daemon). It calls
  `cleanupControlPlane` directly.
- **`DataSession.Cleanup()`** is always the data-plane path (root daemon). It calls
  `cleanupDataPlane` directly.

Both delegate common mechanics (mutex gate, ConfigMapStore stop, 10 s timeout) to
`SessionBase.cleanup`. Cleanup uses a `cleanupMu sync.Mutex` + `cleanedUp bool` (not `sync.Once`)
so that failed cleanups can be retried on the next call. `rollbackFuncList` is protected by
`rollbackMu sync.Mutex` since `AddRollbackFunc` and `getRollbackFuncs` may run concurrently.

```
ConnectOptions.Cleanup() — user daemon (ControlSession), ALWAYS control-plane:
    1. Delete CNI pod/job
    2. Leave all proxy resources (LeaveAllProxyResources)
    3. Execute rollback funcs → session.RunCleanups()
        → removes temp files
        → cancels session context (closes SSH tunnel)
        → detaches logger
    Note: TUN IP is NOT explicitly released — DHCP lease expiry handles reclaim

DataSession.Cleanup() — root daemon, ALWAYS data-plane:
    1. Execute rollback funcs
    2. nm.Stop() → cancel DNS, cancel context
    3. Context cancellation → stops TUN, port-forward, routes
```

## Before & After

### Before (ad-hoc)

```go
sshCtx, sshCancel := context.WithCancel(context.Background())
sshCtx = plog.WithLogger(sshCtx, logger)
defer plog.WithoutLogger(sshCtx)

connect.AddRollbackFunc(func() error {
    sshCancel()
    _ = os.Remove(file)
    return nil
})

defer func() {
    if err != nil {
        connect.Cleanup(...)
        sshCancel()
    }
}()
```

**Problems:**
- `sshCtx` naming misleading when no SSH is involved
- `sshCancel()` scattered in 5+ locations
- `defer plog.WithoutLogger` easy to forget
- `os.Remove(file)` manual and inconsistent
- Not thread-safe, not idempotent

### After (SessionLifecycle)

```go
session := NewSessionLifecycle(logger)
session.AddTempFile(&file)
connect.AddRollbackFunc(func() error {
    session.RunCleanups()
    return nil
})

defer func() {
    if err != nil {
        connect.Cleanup(...)
        session.Cancel()
    }
}()
```

**Improvements:**
- `session.Ctx` — one clear name regardless of SSH or not
- `session.RunCleanups()` — single call handles everything (LIFO, idempotent)
- Logger detach automatic
- Temp file removal via `AddTempFile` (no manual `os.Remove`)
- Thread-safe via mutex

## Source Files

| File | Purpose |
|------|---------|
| `pkg/daemon/action/lifecycle.go` | `SessionLifecycle` struct + `NewSessionLifecycle`, `AddCleanup`, `AddTempFile`, `RunCleanups` |
| `pkg/daemon/action/lifecycle_test.go` | 6 tests: new, cancel, LIFO order, idempotent, temp file, ctx cancelled after cleanup |
| `pkg/daemon/action/connect.go` | Uses `SessionLifecycle` in `Connect` and `redirectConnectToSudoDaemon` |
| `pkg/daemon/action/sync.go` | Uses `SessionLifecycle` in `Sync` |
