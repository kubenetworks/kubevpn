---
name: kubevpn-dev
description: End-to-end workflow for implementing, refactoring, bug-fixing, or testing kubevpn Go code and submitting PRs. Use whenever changing pkg/ or docs/ in this repo, adding tests, or preparing a kubevpn commit/PR.
---

# KubeVPN Development Workflow

Follow these phases in order. Normative detail lives in [AGENTS.md](../../../../AGENTS.md)
(repo root) — each phase links to its section. The detailed reference tables
(architecture map, shared helpers, interface contracts) are in [CLAUDE.md](../../../../CLAUDE.md).

## Phase 1 — Understand before touching code

1. Identify the change area and read its design doc first (AGENTS.md §1 table;
   full index in `docs/README.md`). If your change will contradict the documented
   design, plan to update the doc in the same commit.
2. Map the blast radius: grep for callers of anything you will change, in both
   `.go` and `.md` files. For dual-session code, check **both** planes
   (`ControlSession` user-daemon path AND `DataSession` root-daemon path).
3. Check CLAUDE.md's "Shared Helpers" table before writing any utility — if a
   helper exists, use it; if 3+ call sites duplicate logic, extract one.

## Phase 2 — Implement

Respect the architecture invariants (AGENTS.md §2):

- Control plane vs data plane: put fields/methods on the type that uses them
  (`ConnectOptions` for control, `DataSession` for data, shared plumbing in `SessionBase`).
- Never satisfy an interface with a stub method — use the role interfaces
  (`DataPlane`/`ProxyController` in `pkg/handler/connection.go`) and type-assert.
- Do not edit `cmd/` (frozen) or generated files (`pkg/daemon/rpc/*.pb.go`,
  `pkg/syncthing/auto/gui.files.go`).

Style: match surrounding code; errors lowercase + `%w`; logging via
`plog.G(ctx)`; `k8s.io/utils/ptr`; prefer explicit over implicit.

## Phase 3 — Test (integration-first)

1. Write integration tests that wire up real components against
   `fake.NewSimpleClientset()` and tell a multi-phase story
   (connect → proxy → leave → crash → reconnect), not single-function calls.
2. Anything needing a real cluster goes behind `//go:build integration`
   (`*_integration_test.go`). All other tests must pass with no kubeconfig —
   point any `ConfigFlags` at a temp/dummy path.
3. See AGENTS.md §3 for tag rules and fake.Clientset limitations.

## Phase 4 — Verify

Run in order, fix failures before proceeding (AGENTS.md §4):

```bash
go build ./...
go vet ./pkg/...
go test ./pkg/...
make ut-compile   # also type-check integration-tagged files
```

Before submitting a PR: `make ut` (needs a real cluster/kubeconfig).
This environment has a test cluster at `/data/.kube/config` — switch to context
`kubernetes-admin-cf2096c3e67fb41a0b0cfc3de9a72f027` first (the default `orbstack`
context is unreachable). `TestPing` always fails locally (needs raw sockets).

## Phase 5 — Commit

- One logical change per commit; verify nothing unrelated is staged.
- Update `docs/` design docs and keep `README.md`/`README_ZH.md` in sync if
  user-visible behavior changed.
- Never commit without the Phase 4 loop passing (AGENTS.md §5).
