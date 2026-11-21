
# Using KubeVPN with AI Coding Agents

KubeVPN gives a local process (yours, your debugger's, or an AI agent's) access to a
Kubernetes cluster network, with optional header-scoped traffic interception. This page
is the canonical workflow for AI agents (Claude Code, Cursor, Codex, Copilot, Windsurf)
driving KubeVPN unattended.

## Canonical workflow

1. `kubevpn status --output json` — check whether a connection already exists. If one is
   Connected, reuse it; do not connect again.
2. `kubevpn connect --namespace &lt;ns&gt;` — establish the VPN. Wait for the command to exit
   (it returns once the session is established; the daemon owns the session).
3. `kubevpn proxy --namespace &lt;ns&gt; --headers X-KubeVPN-User=&lt;agent-id&gt; &lt;workload&gt;` —
   intercept the target workload. ALWAYS scope with `--headers`; a bare `proxy` (no
   headers) routes ALL traffic on declared ports to your machine.
4. Verify: request the workload through cluster DNS or service IP from the local process.
5. `kubevpn leave --namespace &lt;ns&gt; &lt;workload&gt;` — restore the workload before finishing.
6. `kubevpn disconnect` — end the VPN session.

`kubevpn mcp` exposes this same workflow as MCP tools (status, connect, proxy, leave,
disconnect, logs) — prefer it when your agent host supports MCP.

## Machine-readable inventory

- `kubevpn status --output json` — full connection state as JSON.
- `kubevpn version` — client version.
- Exit codes: see docs/31-exit-codes.md. Treat non-zero exit as failure; retry at most
  once, then surface the error to the human.

## Hard safety rules for agents

- NEVER run `uninstall`, `reset`, or `quit` without explicit human approval. They affect
  cluster-wide or daemon-wide state.
- ALWAYS `leave` proxy resources before `disconnect` — leave restores the workloads to
  their original spec.
- Prefer header-scoped proxy over bare proxy.
- Scope to one namespace per session.

## Elevation handoff

The first `kubevpn connect` (or any TUN operation) starts a root daemon, which requires
sudo and prompts interactively. An agent MUST NOT retry this blindly: surface the exact
command (`sudo kubevpn daemon`) to the human, wait for them to run it, then retry once.
