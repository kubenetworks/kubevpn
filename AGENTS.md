# AGENTS.md — KubeVPN Contributor Workflow

Tool-neutral guidance for AI coding agents (and humans) working on this repository.
This file is the **workflow entry point**; [CLAUDE.md](CLAUDE.md) holds the detailed
reference tables (shared helpers, architecture map, interface contracts) that the
sections below link into.

- **Language:** Go 1.26+, module `github.com/wencaiwulue/kubevpn/v2`
- **Entry point:** `cmd/kubevpn/main.go`
- **Design docs:** `docs/` (index in [docs/README.md](docs/README.md))

## 1. Before writing any code: read the design docs

Refactoring Rules (see CLAUDE.md) require reading the relevant design documents
**before** any code change, and updating them in the same commit if your change
conflicts with the documented design. Locate the doc by change area:

| Change area | Doc |
|---|---|
| Daemon split / which process runs what | [docs/02-dual-daemon.md](docs/02-dual-daemon.md), [docs/10-handler-architecture.md](docs/10-handler-architecture.md) |
| Envoy rule ownership / multi-user proxy | [docs/05-owner-id.md](docs/05-owner-id.md) |
| TUN IP / DHCP / routes / DNS | [docs/03-dhcp-ip-allocation.md](docs/03-dhcp-ip-allocation.md), [docs/32-cidr-detection.md](docs/32-cidr-detection.md), [docs/19-dns-resolution.md](docs/19-dns-resolution.md) |
| Connection identity / lifecycle | [docs/04-connection-id.md](docs/04-connection-id.md), [docs/12-session-lifecycle.md](docs/12-session-lifecycle.md) |
| Logging | [docs/13-logging-architecture.md](docs/13-logging-architecture.md) |
| Client self-upgrade | [docs/33-client-upgrade.md](docs/33-client-upgrade.md) |
| SSH / privilege escalation | [docs/15-ssh-architecture.md](docs/15-ssh-architecture.md), [docs/34-privilege-escalation.md](docs/34-privilege-escalation.md) |
| File sync / sync mode | [docs/23-syncthing-file-sync.md](docs/23-syncthing-file-sync.md), [docs/26-sync-mode.md](docs/26-sync-mode.md) |
| Something else | Find it in [docs/README.md](docs/README.md) |

## 2. Implementation constraints (architecture invariants)

Full details in [CLAUDE.md](CLAUDE.md) — the "Dual-Session Pattern" and
"Connection Interface Segregation" sections. The load-bearing rules:

- **Two daemon planes, two types.** User daemon (control plane) uses
  `ControlSession` (= `ConnectOptions`, `pkg/handler/connect.go`); root daemon
  (data plane) uses `DataSession` (`pkg/handler/data_session.go`). Both embed
  `SessionBase`. Never re-add stub methods to satisfy an interface — put the
  operation on the owning role interface (`Connection` / `DataPlane` /
  `ProxyController` in `pkg/handler/connection.go`) and type-assert at call sites.
- **`cmd/` is frozen.** No CLI behavior changes. Sole exception: the
  `pkg/core` → `pkg/endpoint` rename in `cmd/kubevpn/cmds/server.go`.
- **Do not re-implement shared helpers.** Check the "Shared Helpers" table in
  CLAUDE.md first (`newStreamWriter`, `resolveKubeconfigBytes`,
  `util.InitFactoryByBytes`, `cleanupConnection`, `handler.DeleteTrafficManagerCoreResources`, …).
- **Generated files are off-limits:** `pkg/daemon/rpc/*.pb.go`,
  `pkg/syncthing/auto/gui.files.go`.
- **Fields that must survive daemon restart need `json:` tags** (only the user
  daemon persists state). K8s client fields live in `K8sClient`/`SessionBase`,
  not on the plane-specific types.

## 3. Tests

- **Every refactoring must include integration tests** — wire up real components
  (gRPC server, DHCP allocator, ConfigMap, connection registry) against
  `fake.NewSimpleClientset()`, structured as multi-phase stories
  (connect → proxy → leave → crash → reconnect). Do not substitute
  single-function unit tests.
- **Build tags:** tests needing a real cluster/kubeconfig go behind
  `//go:build integration` (filename convention `*_integration_test.go`).
  Everything else must run with **no** kubeconfig — never point a
  `genericclioptions.ConfigFlags` at the ambient default kubeconfig. Real-TUN
  tests use the `tun` tag.
- The Windows CI job has no cluster and runs `make ut-no-cluster`; a
  non-`integration` test that touches a live API server breaks it.
- fake.Clientset does not simulate ResourceVersion conflicts — concurrent-write
  tests should assert "no panic + valid result", not exact counts.

## 4. Verification loop

Run after every change, in this order:

```bash
go build ./...        # compile check — run FIRST, fix before continuing
go vet ./pkg/...      # static analysis
go test ./pkg/...     # no-cluster suite (no kubeconfig needed)
make ut-compile       # type-check integration-tagged files too
```

Before submitting: `make ut` (full suite, needs a kubeconfig / real cluster).
Real-TUN tests, on demand: `sudo -E env PATH="$PATH" make ut-tun`.

## 5. Commit & PR rules

- One logical change per commit; don't bundle unrelated fixes.
- Never modify `cmd/` (see exception above).
- File renames use `git mv` to preserve history.
- Keep `README.md` and `README_ZH.md` in sync (bilingual, 1:1 structure);
  user docs live at kubevpn.dev, design docs in this repo's `docs/`.
- Run the verification loop (§4) before every commit; CI runs
  `make ut-no-cluster` on Windows and the full suite on Linux/macOS.

## 6. Code conventions (summary)

Full list in [CLAUDE.md](CLAUDE.md) — the most-violated ones:

- Errors: lowercase, no trailing punctuation, always wrap with `%w`,
  check with `errors.Is`/`errors.As`.
- Logging: `plog.G(ctx)` with import alias
  `plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"` — never bare `log.G(ctx)`.
- Naming: abbreviations all-caps (`UID`, `IP`); `Unpatch` not `UnPatch`;
  `Kubeconfig` one word; multi-word filenames use underscores.
- Deprecations: `k8s.io/utils/ptr` (not `pointer`), `google.golang.org/protobuf/proto`
  (not `github.com/golang/protobuf`), no `github.com/pkg/errors`.

## Skills

`.claude/skills/` follows the open Agent Skills format (plain markdown with
YAML frontmatter), so any agent can follow them directly:

- [`.claude/skills/kubevpn-dev/SKILL.md`](.claude/skills/kubevpn-dev/SKILL.md) —
  end-to-end contribution workflow (docs → implement → integration tests → verify → commit).
- [`.claude/skills/self-improve/SKILL.md`](.claude/skills/self-improve/SKILL.md) —
  how to persist corrections/learnings back into this repo's agent docs.
