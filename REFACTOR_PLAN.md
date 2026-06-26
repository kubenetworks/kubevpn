# KubeVPN Backend Refactoring Plan

> Scope: `pkg/` only. CLI (`cmd/`) is frozen — no changes to command definitions, flags, or user-facing behavior.

## Completed (16 commits)

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
| G     | Cross-cutting        | Fixed all 21 remaining `%v` → `%w` error wrapping                                                           |
| B     | `pkg/core`           | Added underscores to 17 filenames for readability                                                           |
| C     | `pkg/daemon/action`  | Renamed `server.go` → `persistence.go`                                                                      |
| E     | `pkg/localproxy`     | Added debug logging to SOCKS5 error paths                                                                   |

---

## Remaining Work

### Phase A: `pkg/controlplane` — Envoy config generation (Priority: Medium)

**Files:** `cache.go` (501 lines), `processor.go`, `watcher.go`, `server.go`, `controlplane.go`

**Issues:**

- `cache.go:ToListener` is 146 lines — the longest function in the file. Generates envoy Listener proto with complex
  conditional logic for TCP/UDP and fargate mode.
- `cache.go:Virtual.To` is 74 lines — generates 4 different envoy resource types in one function.

**Plan:**

1. Split `ToListener` into `tcpListener` + `udpListener` + fargate variants
2. Extract `Virtual.To` into separate methods per resource type: `toEndpoints()`, `toClusters()`, `toRoutes()`,
   `toListeners()`
3. File rename: ~~`main.go`~~ → already done (`controlplane.go`)

**Risk:** Low — internal only, no external API changes. Envoy proto generation is self-contained.

---

### Phase B: `pkg/core` — Network protocol layer (Priority: Low)

**Files:** 20 files, 2795 lines total

**Issues:**

- File naming is consistent within the package (no underscores, `gvisor*` prefix), but names are long and dense:
  `gvisoricmpforwarder.go`, `gvisorlocaltcphandler.go`
- No major logic issues — protocol implementation is inherently complex
- `tunclient.go` (244 lines) and `tunserver.go` (177 lines) share some buffer handling patterns

**Plan:**

1. Add underscores for readability: `gvisor_icmp_forwarder.go`, `gvisor_tcp_handler.go`, etc.
2. Extract shared TUN read/write buffer logic into `tun_io.go` if pattern duplication is confirmed
3. Leave protocol logic alone — complexity is inherent

**Risk:** Low — pure file renames, no logic changes. The underscore convention improves readability.

---

### Phase C: `pkg/daemon/action` — gRPC action handlers (Priority: Medium)

**Files:** 17 handlers, largest: `connect.go` (240), `logs.go` (213), `status.go` (178)

**Issues:**

- `connect.go:redirectConnectToSudoDaemon` is 132 lines with complexity 23 — long gRPC stream forwarding
- `disconnect.go:Disconnect` has complexity 26 — lots of conditional cleanup logic
- `server.go` mixes config persistence with server state — `LoadFromConfig`, `OffloadToConfig`, `CleanupConfig`

**Plan:**

1. Split `connect.go`: extract `redirectConnectToSudoDaemon` into `connect_elevate.go`
2. In `disconnect.go`: extract the disconnect-one-connection logic from the gRPC stream handling
3. Rename `server.go` → `persistence.go` (it handles config load/save, not server lifecycle)
4. Add entry/exit logs to `LoadFromConfig` and `OffloadToConfig`

**Risk:** Low — each handler is independent.

---

### Phase D: `pkg/dns` — DNS setup (Priority: Low)

**Files:** `dns.go` (287), `dns_unix.go` (252), `dns_linux.go` (202), `dns_windows.go` (104), `forward_server.go` (123)

**Issues:**

- `dns.go:generateAppendHosts` has complex service→host mapping logic
- Platform files have duplicated DNS setup patterns between systemd-resolve and resolvectl
- `dns_unix.go` and `dns_linux.go` overlap — linux IS unix but they're separate

**Plan:**

1. No renames needed — platform suffix convention is correct
2. Extract common DNS entry formatting into shared helper
3. Consider: can `dns_linux.go` extend `dns_unix.go` instead of duplicating?

**Risk:** Medium — DNS changes can break connectivity. Needs integration testing.

---

### Phase E: `pkg/localproxy` — Local proxy server (Priority: Low)

**Files:** `cluster.go` (271), `socks5.go` (132), `server.go` (103), `portforward.go` (87), `httpconnect.go` (60),
`relay.go` (23)

**Issues:**

- `cluster.go` has complexity 24 — main proxy routing logic
- No logging in `socks5.go` handshake errors

**Plan:**

1. Add error logging to SOCKS5 handshake failures
2. Consider splitting `cluster.go` if the function count grows

**Risk:** Low.

---

### Phase F: `pkg/util` — Remaining cleanup (Priority: Medium)

**Files:** 22 files, ~3500 lines

**Issues:**

- `cidr.go` (465 lines) — largest util file, has `CreateCIDRPod` (122 lines, complexity)
- `pod.go` (352 lines) — mixed: PrintStatus, GetEnv, Shell, WaitPod, port-forward
- `util.go` (341 lines) — grab-bag: RolloutStatus, banner printing, Move, If, pprof
- `kubeconfig.go` (251 lines) — kubeconfig operations (already renamed from ns.go)
- Still 21 `fmt.Errorf` with `%v` instead of `%w` for error wrapping

**Plan:**

1. Fix remaining 21 `%v` → `%w` in `fmt.Errorf` for proper error chains
2. Split `pod.go`: extract port-forward helpers into `portforward.go`, keep pod utilities
3. `util.go` is a grab-bag — extract `pprof.go` (StartupPProf functions) and `banner.go` (Print/FormatBanner)
4. `cidr.go`: no split — CIDR detection functions are all related

**Risk:** Low — util changes are mechanical.

---

### Phase G: Cross-cutting — Remaining log and error cleanup (Priority: Medium)

**Issues:**

- 50 remaining `plog.G(context.Background())` — review each: is a real ctx available?
- 21 remaining `fmt.Errorf` with `%v` for errors (should be `%w`)
- Some error messages still start with uppercase

**Plan:**

1. Audit all 50 `context.Background()` log calls — pass ctx where available
2. Fix all 21 `%v` → `%w` error wrapping
3. Scan and fix uppercase error strings in `fmt.Errorf` and `errors.New`

**Risk:** Very low — mechanical changes.

---

### Phase H: `pkg/handler/sync.go` — Sync operations (Priority: Low)

**File:** 546 lines, complexity 28 in `DoSync`

**Issues:**

- `DoSync` is 181 lines — the longest function in handler package
- Mixes K8s operations (clone workload, patch spec) with local Docker setup

**Plan:**

1. Split `DoSync`: extract K8s workload cloning into `cloneWorkload`, local syncthing setup into `setupSyncthing`
2. Consider if `SyncOptions` methods should be in `sync_clone.go` and `sync_local.go`

**Risk:** Medium — sync logic is complex, needs careful testing.

---

## Execution Order (by ROI)

| Priority | Phase                              | Effort | Impact                   | Risk     |
|----------|------------------------------------|--------|--------------------------|----------|
| 1        | G: Cross-cutting log/error cleanup | 1h     | High (debugging)         | Very Low |
| 2        | F: pkg/util remaining cleanup      | 2h     | Medium (readability)     | Low      |
| 3        | A: pkg/controlplane refactor       | 2h     | Medium (maintainability) | Low      |
| 4        | C: pkg/daemon/action cleanup       | 1h     | Medium                   | Low      |
| 5        | H: pkg/handler/sync.go split       | 2h     | Medium                   | Medium   |
| 6        | B: pkg/core file renames           | 30min  | Low (readability)        | Low      |
| 7        | D: pkg/dns deduplication           | 2h     | Low                      | Medium   |
| 8        | E: pkg/localproxy logging          | 30min  | Low                      | Low      |

## What NOT to change

- **`cmd/`** — CLI definitions are frozen
- **`pkg/ssh/gssapi_ccache.go`** — Kerberos credential cache is a protocol implementation, complexity is inherent
- **`pkg/run/docker_opts.go` Parse()** — Docker CLI option parsing, ported from docker/cli
- **`pkg/tun/tun_*.go` createTun()** — Platform-specific TUN creation, complexity is inherent
- **Generated files** (`*.pb.go`, `*.files.go`)
- **Package names** — current package names are clear and appropriate
