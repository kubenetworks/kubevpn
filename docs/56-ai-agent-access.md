# AI Agent Access (Agent Guide + MCP Server)

> Status: **planned** — design agreed, not yet implemented. Two phases: a published
> agent-facing usage guide (docs only), then a `kubevpn-mcp` binary exposing the daemon
> API as MCP tools.

## 1. The Question

mirrord's README (2026-11) opens by addressing AI coding agents by name — Claude Code,
Cursor, Codex, Copilot, Windsurf — and ships an `agents.md` so an agent can onboard the
product unattended. That is a new distribution channel: agents verify code against real
cluster context, which needs exactly what KubeVPN already provides (VPN access + header
routed proxy). KubeVPN has **no agent-facing story today**: its CLI is human-oriented
(interactive elevation prompts, TUI progress output) and nothing tells an agent the safe
workflow.

KubeVPN's architecture makes this cheap: **the user daemon already exposes the full API
as gRPC over a unix socket** (`pkg/daemon/daemon.go`), the CLI is just one client of it,
and `pkg/daemon.GetClient`/`StartupDaemon` already encapsulate connect + auto-start. An
MCP server is a thin, second client — no daemon changes, and no changes to any existing
CLI command (the new `kubevpn mcp` subcommand is additive).

## 2. Deliverables

| Phase | Deliverable | Code? |
|---|---|---|
| P1 | "Using KubeVPN with AI coding agents" guide (kubevpn.dev, mirroring mirrord's agents.md) | none |
| P2 | `kubevpn mcp` subcommand + `pkg/mcp` (stdio MCP server wrapping daemon gRPC) | new subcommand only |

## 3. P1 — Agent-facing Guide

Published on kubevpn.dev (user docs live there, not in `docs/`; this design doc records
the content contract).

Content outline:

1. **Why**: give the agent real cluster context (env vars, service responses, queues)
   without deploying — the same pitch mirrord/Telepresence make, in KubeVPN terms.
2. **Canonical workflow** for an agent: `status` → `connect` (namespace) → `proxy` the
   target workload **with `--headers`** (never bare full-proxy from an agent — header
   scoping keeps other users' traffic unaffected) → verify via cluster DNS/service
   request → `leave` + `disconnect` when done.
3. **Machine-readable inventory**: `kubevpn status --json`, connection list output,
   exit codes (docs/31-exit-codes.md as the reference), and which commands are
   safe/unattended vs need a human (first-run sudo elevation).
4. **Safety rules**, stated as hard constraints for agents:
   - never run `uninstall`, `reset`, or `quit` without explicit human approval;
   - always `leave` proxy resources before `disconnect` (workload restore, docs/36);
   - prefer header-scoped proxy; scope to one namespace.
5. **Elevation handoff**: the first `sudo` daemon start is interactive — the guide tells
   the agent to surface the exact command to the human and wait, not to retry blind.

## 4. P2 — MCP Server

### Entry point and dependency

- `kubevpn mcp` — a **new subcommand** (`cmd/kubevpn/cmds/mcp.go` + registration).
  The frozen-CLI rule protects *existing* commands' parameters/behavior/output; adding a
  new subcommand is allowed. `pkg/mcp` holds the implementation; the command file is a
  thin wiring layer (flags → `pkg/mcp.Run`), same as every other command.
- Dependency: `github.com/modelcontextprotocol/go-sdk` (official Go SDK). **Not in
  `go.mod` today** (measured 2026-11); it is the only new dependency.

### Tools

The server dials the user daemon via `pkg/daemon.GetClient(false)` and maps 1:1 onto
existing RPCs — thin adapters only, no business logic reimplementation.

| Tool | RPC | Annotation | Notes |
|---|---|---|---|
| `kubevpn_status` | `Status` + `ConnectionList` | readOnly | JSON passthrough, compact |
| `kubevpn_logs` | `Logs` (bounded read) | readOnly | last N lines of the selected connection — the agent's debugging channel |
| `kubevpn_connect` | `Connect` (stream) | mutating | resolves once the session is established; returns TUN IP + connection ID summary; accepts namespace/kubeconfig params |
| `kubevpn_proxy` | `Proxy` (stream) | mutating | workloads + optional headers/portmap; tool description mandates header scoping |
| `kubevpn_leave` | `Leave` | mutating | restore workloads for this owner |
| `kubevpn_disconnect` | `Disconnect` | mutating | by connection ID, defaults to current |

**Deliberately excluded**: `Uninstall`, `Reset`, `Upgrade`, `Quit`. The MCP surface is
capability-scoped to the session workflow; destructive/cluster-wide operations stay
human-only (P1 rule 4). This is enforced by omission, not by prompt hygiene.

### Behavioral contract

- **No TTY assumptions**: tool outputs are compact JSON with stable field names; errors
  carry an actionable `hint` (e.g. elevation needed → the exact command to hand the human).
- **Daemon lifecycle**: reuse `StartupDaemon`; if the sudo daemon is missing and elevation
  cannot be granted non-interactively, `kubevpn_connect` fails fast with the handoff error
  rather than hanging — agents must never sit on an interactive prompt.
- **Cancellation**: connect/proxy stream until established, then return; the daemon owns
  the long-lived session (same lifetime contract as the CLI today).
- **Versioning**: MCP server reports its own version and the daemon's (`Version` RPC) so
  an agent can self-diagnose skew.

### Safety model

- Transport is stdio (local agent process), the daemon socket is local — no new network
  surface.
- Mutating tools return the post-action state (`status` snippet) so the agent verifies
  instead of assuming.
- No credential handling: kubeconfig paths are passed through to the daemon's existing
  resolution; the MCP server never prints or stores secrets.

## 5. Testing

- `pkg/mcp` tool handlers against a fake `rpc.DaemonClient` (gRPC in-memory / `net.Pipe()`
  pattern per repo conventions): assert tool → RPC mapping, JSON shape, exclusion of the
  four destructive tools, error-with-hint behavior for elevation.
- No-cluster tests only (`make ut-no-cluster` must stay green); a real end-to-end agent
  run is a manual verification step, not CI.
- P1 is docs-only: verification is the content checklist above.

## 6. Phasing

1. **P1** — guide published on kubevpn.dev; CLI non-interactive gaps found while writing
   it are filed as issues, not patched ad hoc (CLI is frozen).
2. **P2** — `pkg/mcp` + `kubevpn mcp` subcommand + SDK dependency; announce alongside P1 page.

## 7. Related Files

| File | Purpose |
|---|---|
| `cmd/kubevpn/cmds/mcp.go` (new) | `kubevpn mcp` subcommand — thin wiring into `pkg/mcp.Run` |
| `pkg/mcp` (new) | MCP stdio server: tool definitions + daemon gRPC adapters |
| `pkg/daemon/client.go` | `GetClient`/`StartupDaemon` — reused, not reimplemented |
| `pkg/daemon/rpc/daemon.proto` | RPC surface the tools map onto (read-only for this feature) |
| `docs/31-exit-codes.md` | exit-code reference cited by the guide |
| `docs/14-rpc-daemon-mapping.md` | RPC → daemon behavior map, the ground truth for tool docs |
