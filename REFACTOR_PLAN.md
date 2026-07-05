# KubeVPN Backend Refactoring Plan

> Scope: `pkg/` only. CLI (`cmd/`) is frozen — no changes to command definitions, flags, or user-facing behavior.

## Completed

| Phase | Package              | What was done                                                                                               |
|-------|----------------------|-------------------------------------------------------------------------------------------------------------|
| 1     | `pkg/inject`         | Injector interface + Strategy pattern, shared container helpers, PodRouteConfig elimination                 |
| 2     | `pkg/handler`        | Split `connect.go` (1286→421 lines, 5 files), split `remote.go` → `traffmgr.go` + `traffmgr_resources.go`   |
| 3     | `pkg/run`            | PodContext pattern, unified port logic, extracted domain resolution, shared security opts                   |
| 4     | `pkg/ssh`            | Fixed GSSAPI keytab auth bug, fixed copyStream goroutine leak, removed dead code                            |
| 5     | `pkg/daemon/handler` | Fixed race condition (plain map → sync.Map), context propagation                                            |
| 6     | `pkg/webhook`        | Threaded context.Context through admission handlers                                                         |
| 7     | `pkg/util`           | Removed 5 dead functions, unified log alias (log→plog), error string conventions                            |
| 8     | Cross-cutting        | Migrated `pointer.*` → `ptr.To` (22 sites), renamed 12 functions, renamed 8 files, added 14 diagnostic logs |
| A     | `pkg/controlplane`   | Extracted `buildFilterChains`, consolidated xDS helpers, added SchemaVersion/OwnerID                        |
| B     | `pkg/core`           | Added underscores to 17 filenames for readability                                                           |
| C     | `pkg/daemon/action`  | Renamed `server.go` → `persistence.go`, extracted `connect_elevate.go` (sudo redirect logic)                |
| E     | `pkg/localproxy`     | Added debug logging to SOCKS5 error paths                                                                   |
| F     | `pkg/util`           | Extracted `buildCIDRPodSpec`, `serializeKubeconfig`, `PortForwardPod` into dedicated files                  |
| G     | Cross-cutting        | Fixed all `%v` → `%w` error wrapping; audited 31 remaining `context.Background()` (all legitimate)          |
| H     | `pkg/handler`        | Extracted `prepareSyncPodSpec`, `DoSync` reduced from 181→83 lines                                          |
| misc  | Cross-cutting        | Extracted helpers in run, syncthing, handler, core, daemon; comprehensive unit tests across packages        |

---

## Remaining (low priority, deferred)

### Phase D: `pkg/dns` — DNS setup (Priority: Low, Risk: Medium)

**Files:** `dns.go` (287), `dns_unix.go` (252), `dns_linux.go` (202), `dns_windows.go` (104), `forward_server.go` (123)

**Issues:**
- Platform files have duplicated DNS setup patterns between systemd-resolve and resolvectl
- `dns_unix.go` and `dns_linux.go` overlap — linux IS unix but they're separate

**Deferred because:** DNS changes can break connectivity. Needs integration testing which is hard to do safely.

---

## What NOT to change

- **`cmd/`** — CLI definitions are frozen
- **`pkg/ssh/gssapi_ccache.go`** — Kerberos credential cache is a protocol implementation, complexity is inherent
- **`pkg/run/docker_opts.go` Parse()** — Docker CLI option parsing, ported from docker/cli
- **`pkg/tun/tun_*.go` createTun()** — Platform-specific TUN creation, complexity is inherent
- **Generated files** (`*.pb.go`, `*.files.go`)
- **Package names** — current package names are clear and appropriate
