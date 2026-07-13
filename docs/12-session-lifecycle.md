# Context Lifecycle & SessionLifecycle Design

## Overview

KubeVPN daemon actions (Connect, Sync) use `context.Context` to manage the lifecycle of SSH tunnels and VPN connections. The `SessionLifecycle` struct centralizes the session context + teardown, replacing the previous ad-hoc pattern of scattered `ctx/cancel` calls. Teardown is entirely context-cancellation driven — daemon actions no longer materialize temp kubeconfig files (see [15-ssh-architecture.md](15-ssh-architecture.md); kubeconfig stays in memory as bytes), so there is no cleanup registry.

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
}
```

### API

| Method | Purpose |
|--------|---------|
| `NewSessionLifecycle(logger)` | Creates session with `context.Background()` + attached logger |
| `session.Ctx` | Use as the parent context for all session operations |
| `session.Cancel` | Cancel the context (triggers SSH tunnel close, port-forward stop, etc.) |
| `session.Teardown()` | Cancel the context + detach logger. **Idempotent.** Wired into each connection's rollback list. |

### Properties

- **Idempotent**: `Teardown()` can be called multiple times safely (context cancel + logger detach are both idempotent)
- **Context outlives the RPC**: `Ctx` derives from `context.Background()`, so a CLI exit does not tear the session down
- **Automatic logger detach**: `Teardown()` calls `plog.WithoutLogger()` so post-teardown logs fall back to the default logger

## The Three Session Scopes

### 1. Connect — Sudo Daemon (Data Plane)

No SSH tunnel. Kubeconfig is built in-process from bytes (`util.InitFactoryByBytes`) —
no temp kubeconfig file is written.

```
session := NewSessionLifecycle(logger)

ds.AddRollbackFunc(session.Teardown)          ← teardown on disconnect
go grpcutil.ListenCancel(resp, session.Cancel)    ← client cancel → session cancel

ds.InitClient(InitFactoryByBytes(req.KubeconfigBytes, ns))  ← Factory from bytes, no file
ds.DoConnect(session.Ctx)                     ← DataSession, root daemon
    └── ds.ctx = WithCancel(session.Ctx)      ← internal lifecycle
           ├── portForward(ds.ctx)
           ├── startLocalTunServer(ds.ctx)
           ├── addRouteDynamic(ds.ctx)
           └── setupDNS(ds.ctx)

Teardown: session.Cancel() → ds.ctx cancelled → TUN/DNS/routes stop
```

### 2. Connect — User Daemon (Control Plane + SSH)

SSH tunnel to bastion host, then forwards to sudo daemon. The kubeconfig stays in
memory as bytes throughout — no temp file, no bytes→file→bytes round-trip.

```
session := NewSessionLifecycle(logger)

connect.AddRollbackFunc(session.Teardown)          ← teardown on disconnect

resolvedBytes = resolveKubeconfigBytes(session.Ctx, SshJump, ...)  ← SSH tunnel, returns bytes
    └── ssh.SshJumpBytes(session.Ctx, ...)         ← rewritten bytes, NO file
           └── PortMapUntil(session.Ctx, ...)
                  └── go func() { <-session.Ctx.Done(); close tunnel }

connect.InitClient(InitFactoryByBytes(resolvedBytes, ns))   ← Factory from bytes
detectAndSetManagerNamespace(session.Ctx, ...)
connect.InitDHCP(session.Ctx)
forwardConnectToSudo(session.Ctx, ..., resolvedBytes, ...)  ← forwards bytes directly
    ├── req.KubeconfigBytes = string(resolvedBytes)         ← no os.ReadFile round-trip
    ├── req.OwnerID = connect.OwnerID
    └── cli.Connect(ctx)                           ← to sudo daemon

Teardown: session.Cancel() → SSH tunnel closes
```

### 3. Sync

SSH tunnel for kubeconfig (bytes only), independent VPN connection.

```
session := NewSessionLifecycle(logger)

options.AddRollbackFunc(session.Teardown)

cli.Connect(context.Background())                  ← independent! survives sync
resolvedBytes = resolveKubeconfigBytes(session.Ctx, ...)   ← SSH tunnel, no file
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
so that failed cleanups can be retried on the next call. The rollback registry lives in the
embedded `rollbackList` (`pkg/handler/rollback.go`, shared by `SessionBase` and `SyncOptions`);
its internal `mu sync.Mutex` guards `AddRollbackFunc`/`getRollbackFuncs` against concurrent use.

```
ConnectOptions.Cleanup() — user daemon (ControlSession), ALWAYS control-plane:
    1. Delete CNI pod/job
    2. Leave all proxy resources (LeaveAllProxyResources)
    3. Execute rollback funcs → session.Teardown()
        → cancels session context (closes SSH tunnel, port-forwards)
        → detaches logger
       (no temp kubeconfig files to remove — kubeconfig stays in memory as bytes)
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
connect.AddRollbackFunc(func() error {
    session.Teardown()
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
- `session.Teardown()` — single call cancels the context + detaches the logger (idempotent)
- Logger detach automatic
- No temp kubeconfig file to remove (kubeconfig stays in memory as bytes)

## Source Files

| File | Purpose |
|------|---------|
| `pkg/daemon/action/lifecycle.go` | `SessionLifecycle` struct + `NewSessionLifecycle`, `Teardown` |
| `pkg/daemon/action/lifecycle_test.go` | new, cancel, teardown idempotent |
| `pkg/daemon/action/connect.go` | Uses `SessionLifecycle` in `Connect` and `redirectConnectToSudoDaemon` |
| `pkg/daemon/action/sync.go` | Uses `SessionLifecycle` in `Sync` |
