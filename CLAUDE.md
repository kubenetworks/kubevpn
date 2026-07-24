# CLAUDE.md — KubeVPN Project Instructions

## Project Overview

KubeVPN is a CLI tool that connects local development environments to Kubernetes cluster networks. It creates TUN
devices, manages DHCP IP allocation, injects Envoy sidecar proxies, and supports SSH jump hosts.

- **Language:** Go 1.26+
- **Module:** `github.com/wencaiwulue/kubevpn/v2`
- **Entry point:** `cmd/kubevpn/main.go`

## Build & Test

```bash
# Build
go build ./...              # verify compilation
make kubevpn                # build binary with ldflags

# Test
go test ./pkg/...           # run no-cluster tests (no kubeconfig needed)
go test -tags=integration ./pkg/...  # also run tests that need a real cluster
go test ./pkg/inject/... -v # test specific package
go vet ./pkg/...            # static analysis

make ut             # FULL suite incl. cluster tests (-tags=integration); needs kubeconfig
make ut-no-cluster  # ONLY no-kubeconfig tests; used by the Windows CI job

# Note: TestPing always fails in this environment (needs raw socket)
# Note: CGO_ENABLED=0, so -race is unavailable

# Generate protobuf
make gen
```

### Test Build Tags (kubeconfig vs no-kubeconfig)

Tests that need a real Kubernetes cluster (and therefore a kubeconfig) live behind
the **`integration`** build tag — `//go:build integration` at the top of the file.
Everything without that tag must run with NO kubeconfig (use `fake.NewSimpleClientset()`,
`httptest`, temp/dummy kubeconfig files, or `net.Pipe()`). The Windows CI job has no
cluster and runs `make ut-no-cluster`, so:

- **Never** make a non-`integration` test call a real cluster API (`ToRESTConfig()` +
  real Get/List, `kubernetes.NewForConfig` against a live server). If it needs a cluster,
  move it to a `//go:build integration` file (e.g. `*_integration_test.go`).
- A non-`integration` test that builds a `genericclioptions.ConfigFlags` MUST point
  `configFlags.KubeConfig` at a temp/dummy/nonexistent path — never the ambient default.
- Other tag in use: `tun` for real-TUN tests needing `CAP_NET_ADMIN` (run on demand).

## Test Cluster

A test cluster is available at `/data/.kube/config`:

- Context: `kubernetes-admin-cf2096c3e67fb41a0b0cfc3de9a72f027` (Alibaba Cloud ACK)
- The default context `orbstack` is local and unreachable — always switch to the remote context first

## Architecture

```
cmd/kubevpn/cmds/     CLI command definitions (DO NOT MODIFY in refactoring)
pkg/
├── config/            Constants, image config, syncthing paths
├── controlplane/      Envoy xDS control plane (gRPC, config cache)
├── core/              Network protocol core (TUN, gvisor, TCP/UDP forwarding)
├── cp/                File copy (kubectl cp equivalent)
├── daemon/            gRPC daemon server
│   ├── action/        Per-command daemon handlers (connect, proxy, leave, etc.)
│   │   ├── connection.go  Connection lookup/remove helpers (findConnection, removeConnection, cleanupConnection)
│   │   ├── lifecycle.go   SessionLifecycle — session context + Teardown (cancel + logger detach) for daemon sessions
│   │   └── writer.go      newStreamWriter, initStreamLogger, resolveKubeconfigBytes
│   ├── handler/       WebSocket SSH terminal handler
│   ├── elevate/       Privilege escalation (sudo/admin)
│   └── rpc/           Generated protobuf (DO NOT EDIT *.pb.go)
├── dhcp/              DHCP IP allocation via ConfigMap
├── dns/               DNS setup (platform-specific: linux/unix/windows)
├── driver/            TUN/TAP driver management (wintun, openvpn)
├── handler/           Core business logic
│   ├── session_base.go    SessionBase — shared base (K8sClient + rollbackList + cleanup) embedded by ConnectOptions & DataSession
│   ├── connect.go         ConnectOptions (= ControlSession) — control-plane methods + data-plane stubs
│   ├── control_session.go type ControlSession = ConnectOptions (alias)
│   ├── data_session.go    DataSession — data-plane methods, DoConnect, cleanupDataPlane
│   ├── connection.go      Connection (shared) + DataPlane/ProxyController role interfaces + compile-time assertions (Interface Segregation)
│   ├── rollback.go        rollbackList — mutex-guarded rollback registry (embedded by SessionBase & SyncOptions)
│   ├── cleaner.go         ConnectOptions.Cleanup + cleanupControlPlane + executeRollbackFuncs
│   ├── connect_tun.go     TUN server, port forwarding, health checks
│   ├── connect_route.go   Dynamic routing, extra routes, watchAndRoute
│   ├── connect_dns.go     DNS setup
│   ├── connect_upgrade.go Traffic manager deployment upgrade
│   ├── network.go         NetworkManager — owns full networking lifecycle (port-forward, TUN IP allocation, routes, DNS)
│   ├── k8s_client.go      K8sClient embedded struct (via SessionBase → ConnectOptions/DataSession/SyncOptions)
│   ├── traffmgr.go        Create traffic manager pod
│   ├── traffmgr_resources.go  K8s resource generators (deploy, svc, secret, etc.)
│   ├── leave.go           Leave/unpatch proxy resources
│   ├── proxy.go           Port mapping management
│   ├── proxy_mapper.go    Mapper for port-forward config from ConfigMap
│   ├── sync.go            Syncthing-based file sync
│   └── reset.go           Reset workloads to original spec
├── inject/            Sidecar injection (Injector interface + Strategy pattern)
│   ├── injector.go        Interface, factory, shared helpers
│   ├── vpn.go             VPN-only injector
│   ├── mesh.go            Envoy+VPN injector + UnpatchContainer
│   ├── fargate.go         Envoy+SSH injector (Fargate/Service mode)
│   ├── container.go       Container builders (shared helpers)
│   └── envoy.go           Envoy ConfigMap CRUD, template rendering
├── localproxy/        Local SOCKS5/HTTP proxy server
├── log/               Structured logging (context-based)
├── run/               `kubevpn run` — run K8s workloads in local Docker
│   ├── options.go         Options struct, Main, Run, PodContext
│   ├── connect.go         Cluster connection (host/container mode)
│   ├── runconfig.go       Pod→Docker config conversion
│   ├── docker_opts.go     Docker CLI flag parsing
│   └── docker_utils.go    Shared Docker helpers
├── ssh/               SSH client, jump hosts, GSSAPI auth
├── syncthing/         Syncthing client/server integration
├── tun/               TUN device creation and route management
├── upgrade/           Client self-upgrade
└── util/              Shared utilities
```

## Generated Files — DO NOT EDIT

- `pkg/daemon/rpc/*.pb.go` — protobuf generated
- `pkg/syncthing/auto/gui.files.go` — embedded assets

## Code Conventions

### Go Style

- Handle errors explicitly (`if err != nil`), use `context` for cancellation
- Error strings: lowercase, no trailing punctuation (`fmt.Errorf("failed to connect: %w", err)`)
- Always use `%w` (not `%v`) when wrapping errors with `fmt.Errorf`
- Use `errors.Is`/`errors.As` for error checking
- Use correct English grammar: `"cannot find"` (not `"can not found"`)
- Use named sentinel errors for control flow: `var errPodCreated = errors.New("pod created")`

### Logging

- Use `plog.G(ctx)` (import alias `plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"`)
- NEVER use `log.G(ctx)` without the `plog` alias — the project standard is `plog`
- Use `context.Background()` only in goroutines/init where no ctx is available
- Log levels: `Debug` for internal tracing, `Info` for user-visible operations, `Error` for failures

### Naming

- Abbreviations: all-caps (`UID`, not `Uid`; `IP`, not `Ip`)
- Compound words: `Unpatch` not `UnPatch`, `Kubeconfig` not `KubeConfig` (when one word)
- Functions should describe intent, not implementation (`RecreatePod` not `CreateAfterDeletePod`)
- File names: use underscores for multi-word names (`gvisor_tcp_handler.go`)

### Deprecated APIs

- Use `k8s.io/utils/ptr` (not `k8s.io/utils/pointer`) — `ptr.To(value)` instead of `pointer.Bool()`
- Use `google.golang.org/protobuf/proto` (not `github.com/golang/protobuf/proto`)
- Use `fmt.Errorf("...: %w", err)` (not `github.com/pkg/errors`) — fully migrated, zero files import it

### Design Patterns in Use

- **Strategy pattern** in `pkg/inject` — `Injector` interface with `NewInjector` factory
- **Strategy pattern** in `pkg/core` — `stackConstructor` function type for gvisor stack creation
- **Embedded struct** in `pkg/handler` — `K8sClient` bundles (clientset, restclient, config, factory) + `InitClient`/`GetFactory`/`GetClientset` methods; embedded via `SessionBase` by `ConnectOptions`/`DataSession`, and directly by `SyncOptions`
- **Session lifecycle** in `daemon/action` — `SessionLifecycle` owns the daemon session context + `Teardown()` (cancel + logger detach); the rollback/cleanup registry lives separately in `handler.rollbackList` + `SessionBase.cleanup`
- **Rollback registry** in `pkg/handler` — `rollbackList` (mutex-guarded `AddRollbackFunc`/`getRollbackFuncs`) embedded by `SessionBase` and `SyncOptions`; run snapshots via `executeRollbackFuncs`
- **Parameter object** in `pkg/inject` — `envoyRuleSpec` struct replaces 10+ individual args to `addEnvoyConfig`/`addVirtualRule`
- **PodContext bundle** in `pkg/run` — groups K8s-fetched data (template, env, volume, DNS)
- **Registry pattern** in `pkg/daemon/handler` — `sync.Map`-backed SSH session registry

### Shared Helpers (avoid re-implementing these)

| Helper | Package | Purpose |
|---|---|---|
| `newStreamWriter(send)` | `daemon/action` | Adapts gRPC streaming Send into `io.Writer` — do NOT create per-action wrapper structs |
| `svr.initStreamLogger(resp, level, sendMsg)` | `daemon/action` | Creates logger writing to both gRPC stream and log file, returns (logger, ctx) — used by reset, leave, uninstall, unsync |
| `resolveKubeconfigBytes(ctx, jump, bytes, portForward)` | `daemon/action` | SSH jump + returns in-memory kubeconfig bytes (NO temp file) — feed to `util.InitFactoryByBytes`; do NOT inline the SSH/kubeconfig pattern |
| `util.InitFactoryByBytes(bytes, ns)` | `pkg/util` | Builds a kubectl Factory directly from kubeconfig bytes (in-memory RESTClientGetter) — prefer over `InitFactoryByPath` when the Factory is consumed in-process (no child/mount/env needs a file) |
| `ssh.SshJumpBytes(ctx, conf, bytes, print)` | `pkg/ssh` | Establishes the SSH tunnel and returns rewritten kubeconfig bytes (no file); `SshJump` is the file-materializing wrapper for child-process/KUBECONFIG consumers |
| `NewSessionLifecycle(logger)` | `daemon/action` | Session context + `Teardown()` (cancel + logger detach) — replaces ad-hoc context.WithCancel; rollback/cleanup is NOT here (see `handler.rollbackList`) |
| `cleanupConnection(ctx, conn)` | `daemon/action` | Cleans up a connection's sync and VPN state — used by disconnect and quit |
| `util.InitKubeClient(f)` | `pkg/util` | Returns (config, restclient, clientset, namespace) — used by `K8sClient.InitClient` |
| `util.IsNewer(clientVer, clientImg, serverImg)` | `pkg/util` | Version/image-tag comparison (MAJOR.MINOR, tolerant of SHA/latest) — used by client self-upgrade and traffic-manager `UpgradeDeploy` |
| `gatherContainerPorts(spec, portMaps)` | `pkg/inject` | Collects container ports from pod spec + portMaps — shared by mesh and fargate |
| `addEnvoyConfig(ctx, mapInterface, spec)` | `pkg/inject` | Adds envoy proxy rule to ConfigMap using `envoyRuleSpec` — shared by vpn and fargate injectors |
| `svr.findConnection(id)` | `daemon/action` | Finds connection by ID — do NOT write `for range svr.connections` lookup loops |
| `svr.removeConnection(id)` | `daemon/action` | Removes connections by ID from slice, returns them for caller cleanup |
| `svr.resetCurrentConnection(id)` | `daemon/action` | After removing a connection, picks first remaining as current |
| `svr.getSudoTunIPs(ctx)` | `daemon/action` | Queries sudo daemon Status, returns `map[ConnectionID]tunIP` — use for IP lookup in user daemon |
| `resolveTunIP(connect, ips)` | `daemon/action` | Resolves TUN IP for a connection: from map (user daemon) or `GetLocalTunIP` fallback (sudo daemon) |
| `handler.DeleteTrafficManagerCoreResources(ctx, clientset, ns, name, opts)` | `pkg/handler` | Deletes the 8 namespaced traffic-manager resources (deploy/job/svc/configmap/secret/sa/role/rolebinding) — shared by `cleanupTrafficManagerResources` (handler) and `Uninstall` (daemon/action); do NOT re-list these resources in either caller |

### Envoy Config Types

- **`controlplane.Virtual`** — per-workload envoy xDS config stored in ConfigMap. Has `SchemaVersion` field (current: `controlplane.CurrentSchemaVersion = 2`; zero = legacy pre-versioning, requires OwnerID on all rules)
- **`controlplane.PortMapping`** — parsed representation of the `Rule.PortMap` string encoding. Use `rule.ParsePortMap()` instead of manually parsing the `"envoyPort:localPort"` strings
- **`inject.envoyRuleSpec`** — parameter object for `addEnvoyConfig`/`addVirtualRule`. Groups Namespace, NodeID, IPs, Headers, Ports, PortMap, FargateMode, OwnerID

### Fargate/Service Mode

- Detection: use `virtual.IsFargateMode()` which checks the explicit `FargateMode` bool field first
- Do NOT infer mode from `EnvoyListenerPort != 0` — that is a legacy fallback only
- Port names: use `config.PortNameTCP`, `config.PortNameEnvoy`, `config.PortNameHTTP`, `config.PortNameDNS`
- Rollback functions: always `AddRollbackFunc`
- Design doc: `docs/06-fargate-mode.md`

### Dual-Session Pattern: ControlSession vs DataSession (IMPORTANT)

**Full architecture docs: `docs/02-dual-daemon.md`, `docs/10-handler-architecture.md`**

The two daemon layers use **two distinct types** (not one struct with an `isDataPlane` flag),
both embedding `SessionBase` (K8sClient + rollbackList + cleanup). `Cleanup` dispatches by
type — no runtime flag:

| | User Daemon (control plane) | Root Daemon (data plane) |
|---|---|---|
| Type | `ControlSession` (= `ConnectOptions`, `connect.go`/`cleaner.go`) | `DataSession` (`data_session.go`) |
| Init path | `redirectConnectToSudoDaemon` → `forwardConnectToSudo` | `Connect` (IsSudo=true) → `ds.DoConnect` |
| Cleanup | `cleanupControlPlane` | `cleanupDataPlane` |
| Role | traffic manager create/upgrade, proxy inject, health check | TUN, IP allocation (rentIP), port-forward, DNS, routes, CIDR detection |
| TUN IP | query the sudo daemon via `getSudoTunIPs` | `NetworkManager.localTunIPv4/v6` (allocated by rentIP) |
| Persisted | ✅ OffloadToConfig | ❌ |

**Connect flow (control plane → data plane):**
```
User Daemon: CreateOutboundPod → UpgradeDeploy → cli.Connect(ctx) [req.OwnerID]
Root Daemon: ds.OwnerID = req.OwnerID → ds.DoConnect (getCIDR → NetworkManager.Start → rentIP)
```

**How the User Daemon obtains the TUN IP:**
- `ControlSession` does not hold `LocalTunIPv4/v6`
- When it needs an IP, it calls `svr.getSudoTunIPs(ctx)` to query the sudo daemon's Status RPC
- It matches the IP for each connection by ConnectionID via `resolveTunIP(connect, ips)`
- Used by: sidecar injection (`CreateRemoteInboundPod`), leave, and status queries

**Rules when modifying these types:**
- **K8s client fields live in `K8sClient`** (embedded via `SessionBase`) — holds clientset, restclient, config, factory. Use `GetFactory()`/`GetClientset()`. Initialize via `InitClient(f)` (delegates to `util.InitKubeClient`); build the factory with `util.InitFactoryByBytes` (no temp file)
- **Put the field on the type that uses it** — control-plane fields on `ConnectOptions`, data-plane fields on `DataSession`. Shared non-identity plumbing (K8sClient, rollbackList, configMapStore) goes in `SessionBase`
- **`DoConnect` is a `DataSession` method** (a stub on `ConnectOptions`); it runs in the Root Daemon
- **Traffic manager pod create/upgrade is a control-plane responsibility** — `CreateOutboundPod`/`UpgradeDeploy` are called in the User Daemon's `forwardConnectToSudo`; `DoConnect` does not handle them
- Fields that need to survive daemon restart must have `json:` tags (only User Daemon persists)
- Test both paths: root daemon (via `DataSession.DoConnect`) and user daemon (via `forwardConnectToSudo`)

### ConnectionID

- ConnectionID = last 12 hex chars of namespace UID, identifies a VPN connection
- **Generated by user daemon** in `connect_elevate.go` via `util.GetConnectionID()`, stored as `connect.ConnectionID`
- **Transmitted to root daemon** via `ConnectRequest.ConnectionID` proto field
- Root daemon receives and stores it: `connect.ConnectionID = req.ConnectionID`
- Used as primary key for `findConnection`, `removeConnection`, disconnect, status queries
- Design doc: `docs/04-connection-id.md`

### Connection Interface Segregation (DataPlane / ProxyController)

`pkg/handler/connection.go` defines THREE interfaces, not one:

- **`Connection`** — the shared identity/lifecycle/status surface **both** `*ConnectOptions` and `*DataSession` satisfy. `Server.connections` is typed `[]Connection`.
- **`DataPlane`** (embeds `Connection`) — root-daemon-only operations: `DoConnect`, `GetLocalTunIP`, `GetAPIServerIPs`, `GetNetworkExtraHost`, `GetLastHeartbeat`. Only `*DataSession` satisfies it.
- **`ProxyController`** (embeds `Connection`) — user-daemon-only operations: `CreateRemoteInboundPod`, `LeaveAllProxyResources`, `LeaveResource`, `ProxyResources`, `SetSync`. Only `*ConnectOptions` satisfies it.

This replaced the old single 28-method `Connection` interface, where each type carried stubs for the other plane's methods (e.g. `ConnectOptions.DoConnect` returned an error, `DataSession.CreateRemoteInboundPod` was a no-op) — a wrong-plane call compiled and failed at runtime. Now the type system rejects it at compile time.

**Rules when modifying these interfaces:**
- Consumers that need plane-specific behavior **type-assert** (`if dp, ok := conn.(handler.DataPlane); ok { ... }`), with a zero-value fallback that preserves the old stub behavior. See `resolveTunIP`, `resolveStatus`, `sort.Connects.Less`, and the disconnect/leave/unsync/proxy handlers in `daemon/action`.
- Never re-add a "stub" method to satisfy an interface — if only one plane owns an operation, put it on the matching role interface and have callers assert.
- `GetSync`, `GetSocksListenAddr`, `GetSocksEgress` stay on shared `Connection` because their zero value (`nil`/`""`/`false`) is a meaningful "not applicable on this plane" return, not a stub.

## Integration Testing

**Every refactoring must include integration tests.** When writing tests, always default to integration tests that wire up multiple real components end-to-end — never write unit tests that call a single function in isolation.

Integration tests wire up real components (gRPC server, DHCP allocator, ConfigMap, envoy rules) against `fake.NewSimpleClientset()` to test multi-user scenarios without a real cluster.

### Test Infrastructure

| Component | How to set up | Example |
|---|---|---|
| TunConfigServer + gRPC | `newTestEnv(t)` in `controlplane/integration_test.go` | Returns `env.client` (gRPC client) + `env.server` (direct access) |
| ConfigMap envoy rules | `fake.NewSimpleClientset(&v1.ConfigMap{...})` | Read-modify-write via `clientset.CoreV1().ConfigMaps(ns)` |
| Connection management | Direct `Server{}` struct | `svr.findConnection()`, `svr.removeConnection()` |
| Fake K8s client | `fake.NewSimpleClientset(objects...)` | Supports Get/Update/Patch/List for ConfigMap, Namespace, Secret, Pod |

### Writing Multi-User Integration Tests

Multi-user tests should simulate the **full lifecycle** across multiple components, not just call individual functions:

```go
// Good: integration test — wires up DHCP + envoy + connection state
func TestIntegration_MultiUser_FullLifecycle(t *testing.T) {
    env := newTestEnv(t)  // starts real TunConfigServer + gRPC
    // Phase 1: allocate IPs via gRPC
    resp, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "alice"})
    // Phase 2: write envoy rules to ConfigMap
    // Phase 3: one user leaves → verify others unaffected
    // Phase 4: crash recovery → same OwnerID, new IP
}

// Bad: unit test disguised as integration — calls one function in isolation
func TestAddEnvoyConfig(t *testing.T) {
    addEnvoyConfig(ctx, mapInterface, spec)  // just one function call
}
```

### Key Test Patterns

**Multi-user DHCP interaction** (`controlplane/multiuser_integration_test.go`):
- Use `env.client.GetTunIP()` for each user (real gRPC)
- Verify IPs are unique across users
- Force-expire one user's lease → verify other's IP unchanged
- WatchTunIP stream keeps lease alive vs idle user expiry

**Multi-user envoy rules** (`inject/multiuser_test.go`, `inject/multiuser_interaction_test.go`):
- User A + B proxy same workload with different headers → 2 rules coexist
- Same headers → ownership takeover (Case 3 in `addVirtualRule`)
- One user leaves → other's rule untouched; last user leaves → Virtual removed
- Crash recovery: same OwnerID re-proxy → rule updated, not duplicated

**Fault injection** (`core/fault_injection_test.go`):
- `net.Pipe()` + `Close()` to simulate TCP disconnect
- `flakyConn` wrapper to fail after N writes
- Real TCP listener for read deadline timeout tests
- `context.WithCancel` for abrupt shutdown, verify channel drain

### fake.Clientset Limitations

- Does NOT simulate ResourceVersion conflicts — concurrent `Update()` calls overwrite each other silently
- `retry.RetryOnConflict` never retries (no conflict errors)
- Concurrent write tests should verify "no panic + valid YAML", not exact counts
- Full concurrent correctness requires real K8s API (CI with minikube)

## Refactoring Rules

When refactoring backend code (`pkg/`):

0. **Read `docs/` design documents first** — before any code change, read the relevant design docs in `docs/` to understand the full architecture. Changes must be consistent with the documented design (dual-daemon model, OwnerID ownership, DHCP lifecycle, logging architecture, etc.). If a change conflicts with the docs, update the docs as part of the same commit. Key docs: `02-dual-daemon.md` (which daemon runs what), `05-owner-id.md` (envoy rule ownership), `13-logging-architecture.md` (log routing), `14-rpc-daemon-mapping.md` (RPC → daemon mapping), `33-client-upgrade.md` (client self-upgrade: download/atomic-swap/rollback).
1. **Every refactoring must include integration tests** — wire up real components (gRPC, DHCP, ConfigMap, connection management) against `fake.NewSimpleClientset()`. Structure tests as multi-phase stories (connect → proxy → leave → crash → reconnect). Never write single-function unit tests as a substitute.
2. **Never touch `cmd/`** — CLI is frozen
3. **Always `go build ./...` after changes** — catch compile errors immediately
4. **Run `go test` and `go vet`** on affected packages before committing
5. **Commit per logical change** — don't bundle unrelated fixes
6. **Inherent complexity is OK** — don't split protocol/platform code just to reduce line count
7. **File renames need `git mv`** — preserve git history
8. **Prefer explicit over implicit** — use named fields/types instead of magic value checks
9. **Extract shared helpers** — if 3+ call sites duplicate logic, extract to a shared function
10. **Any modification: verify global impact** —
   - Inserting code in a function: re-read the entire function's control flow
   - Removing a json tag: grep all deserialization paths that read that field
   - Deleting a file/package: grep all `.go` AND `.md` for references
   - Removing all callers of an API: also delete the API implementation
   - Extracting new types: default to unexported, export only when external packages need it
   - Writing a TODO: if it blocks correctness, fix it now; never ship broken code with a TODO
