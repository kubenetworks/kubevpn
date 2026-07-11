# CLAUDE.md — KubeVPN Project Instructions

## Project Overview

KubeVPN is a CLI tool that connects local development environments to Kubernetes cluster networks. It creates TUN
devices, manages DHCP IP allocation, injects Envoy sidecar proxies, and supports SSH jump hosts.

- **Language:** Go 1.23+
- **Module:** `github.com/wencaiwulue/kubevpn/v2`
- **Entry point:** `cmd/kubevpn/main.go`

## Build & Test

```bash
# Build
go build ./...              # verify compilation
make kubevpn                # build binary with ldflags

# Test
go test ./pkg/...           # run all package tests
go test ./pkg/inject/... -v # test specific package
go vet ./pkg/...            # static analysis

# Note: TestPing always fails in this environment (needs raw socket)
# Note: CGO_ENABLED=0, so -race is unavailable

# Generate protobuf
make gen
```

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
│   │   ├── lifecycle.go   SessionLifecycle — context + LIFO cleanup manager for daemon sessions
│   │   └── writer.go      newStreamWriter, initStreamLogger, resolveKubeconfig
│   ├── handler/       WebSocket SSH terminal handler
│   ├── elevate/       Privilege escalation (sudo/admin)
│   └── rpc/           Generated protobuf (DO NOT EDIT *.pb.go)
├── dhcp/              DHCP IP allocation via ConfigMap
├── dns/               DNS setup (platform-specific: linux/unix/windows)
├── driver/            TUN/TAP driver management (wintun, openvpn)
├── handler/           Core business logic
│   ├── connect.go         ConnectOptions struct + DoConnect orchestration
│   ├── connect_tun.go     TUN server, port forwarding, health checks
│   ├── connect_route.go   Dynamic routing, extra routes, watchAndRoute
│   ├── connect_dns.go     DNS setup
│   ├── connect_upgrade.go Traffic manager deployment upgrade
│   ├── network.go         NetworkManager — owns full networking lifecycle (port-forward, TUN IP allocation, routes, DNS)
│   ├── k8s_client.go      K8sClient embedded struct (shared by ConnectOptions/SyncOptions)
│   ├── healthchecker.go   HealthStatus, periodic health check loop
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
├── util/              Shared utilities
└── webhook/           Admission webhook (DHCP IP injection)
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
- **Embedded struct** in `pkg/handler` — `K8sClient` bundles (clientset, restclient, config, factory) + `InitClient`/`GetFactory`/`GetClientset` methods; embedded by `ConnectOptions` and `SyncOptions`
- **Session lifecycle** in `daemon/action` — `SessionLifecycle` manages context + LIFO cleanup stack for daemon RPC sessions
- **Parameter object** in `pkg/inject` — `envoyRuleSpec` struct replaces 10+ individual args to `addEnvoyConfig`/`addVirtualRule`
- **PodContext bundle** in `pkg/run` — groups K8s-fetched data (template, env, volume, DNS)
- **Registry pattern** in `pkg/daemon/handler` — `sync.Map`-backed SSH session registry

### Shared Helpers (avoid re-implementing these)

| Helper | Package | Purpose |
|---|---|---|
| `newStreamWriter(send)` | `daemon/action` | Adapts gRPC streaming Send into `io.Writer` — do NOT create per-action wrapper structs |
| `svr.initStreamLogger(resp, level, sendMsg)` | `daemon/action` | Creates logger writing to both gRPC stream and log file, returns (logger, ctx) — used by reset, leave, uninstall, unsync |
| `resolveKubeconfig(ctx, jump, bytes, portForward)` | `daemon/action` | SSH jump + kubeconfig file resolution — do NOT inline the SSH/kubeconfig pattern |
| `NewSessionLifecycle(logger)` | `daemon/action` | Context + LIFO cleanup manager for daemon sessions — replaces ad-hoc context.WithCancel + scattered cleanup |
| `cleanupConnection(ctx, conn)` | `daemon/action` | Cleans up a connection's sync and VPN state — used by disconnect and quit |
| `util.InitKubeClient(f)` | `pkg/util` | Returns (config, restclient, clientset, namespace) — used by `K8sClient.InitClient` |
| `gatherContainerPorts(spec, portMaps)` | `pkg/inject` | Collects container ports from pod spec + portMaps — shared by mesh and fargate |
| `addEnvoyConfig(ctx, mapInterface, spec)` | `pkg/inject` | Adds envoy proxy rule to ConfigMap using `envoyRuleSpec` — shared by vpn and fargate injectors |
| `svr.findConnection(id)` | `daemon/action` | Finds connection by ID — do NOT write `for range svr.connections` lookup loops |
| `svr.removeConnection(id)` | `daemon/action` | Removes connections by ID from slice, returns them for caller cleanup |
| `svr.resetCurrentConnection(id)` | `daemon/action` | After removing a connection, picks first remaining as current |
| `svr.getSudoTunIPs(ctx)` | `daemon/action` | Queries sudo daemon Status, returns `map[ConnectionID]tunIP` — use for IP lookup in user daemon |
| `resolveTunIP(connect, ips)` | `daemon/action` | Resolves TUN IP for a connection: from map (user daemon) or `GetLocalTunIP` fallback (sudo daemon) |

### Envoy Config Types

- **`controlplane.Virtual`** — per-workload envoy xDS config stored in ConfigMap. Has `SchemaVersion` field (current: `controlplane.CurrentSchemaVersion = 1`; zero = legacy pre-versioning)
- **`controlplane.PortMapping`** — parsed representation of the `Rule.PortMap` string encoding. Use `rule.ParsePortMap()` instead of manually parsing the `"envoyPort:localPort"` strings
- **`inject.envoyRuleSpec`** — parameter object for `addEnvoyConfig`/`addVirtualRule`. Groups Namespace, NodeID, IPs, Headers, Ports, PortMap, FargateMode, OwnerID

### Fargate/Service Mode

- Detection: use `virtual.IsFargateMode()` which checks the explicit `FargateMode` bool field first
- Do NOT infer mode from `EnvoyListenerPort != 0` — that is a legacy fallback only
- Port names: use `config.PortNameTCP`, `config.PortNameEnvoy`, `config.PortNameHTTP`, `config.PortNameDNS`
- Rollback functions: always `AddRollbackFunc` (not `AddRolloutFunc`)
- Design doc: `docs/06-fargate-mode.md`

### ConnectOptions Dual-Role Pattern (IMPORTANT)

**Full architecture doc: `docs/02-dual-daemon.md`**

`ConnectOptions` is used in BOTH daemon layers with **independent instances** (they do NOT share memory):

| | User Daemon (control plane) | Root Daemon (data plane) |
|---|---|---|
| Init path | `redirectConnectToSudoDaemon` | `Connect` (IsSudo=true) → `DoConnect` |
| `isDataPlane` | false | true (set by DoConnect) |
| Role | traffic manager 创建/升级, proxy inject, health check | TUN, IP 分配 (rentIP), port-forward, DNS, routes, CIDR 探测 |
| TUN IP | 通过 `getSudoTunIPs` 查询 sudo daemon | `NetworkManager.localTunIPv4/v6` (rentIP 分配) |
| Persisted | ✅ OffloadToConfig | ❌ |

**Connect 流程（控制面 → 数据面）:**
```
User Daemon: CreateOutboundPod → UpgradeDeploy → cli.Connect(ctx) [req.OwnerID]
Root Daemon: connect.OwnerID = req.OwnerID → DoConnect (getCIDR → NetworkManager.Start → rentIP)
```

**User Daemon 获取 TUN IP:**
- User daemon 不再持有 `LocalTunIPv4/v6` 字段
- 需要 IP 时调用 `svr.getSudoTunIPs(ctx)` 查询 sudo daemon 的 Status RPC
- 通过 `resolveTunIP(connect, ips)` 按 ConnectionID 匹配对应连接的 IP
- 用于：sidecar 注入 (`CreateRemoteInboundPod`)、leave、status 查询

**Rules when modifying `ConnectOptions`:**
- **K8s client fields live in `K8sClient`** — the embedded struct in `k8s_client.go` holds clientset, restclient, config, factory. Use `GetFactory()` / `GetClientset()` accessors. Initialize via `InitClient(f)` which delegates to `util.InitKubeClient`
- **Determine which daemon uses the field FIRST** — control-plane fields go in `redirectConnectToSudoDaemon`, data-plane fields go in `DoConnect`
- **NEVER initialize control-plane fields in DoConnect** — DoConnect runs in Root Daemon where those fields are unused
- **NEVER initialize data-plane fields in redirectConnectToSudoDaemon** — User Daemon doesn't create TUN devices
- **Traffic manager pod 创建/升级是控制面职责** — `CreateOutboundPod` 和 `UpgradeDeploy` 在 User Daemon 的 `forwardConnectToSudo` 中调用，`DoConnect` 不再负责
- Fields that need to survive daemon restart must have `json:` tags (only User Daemon persists)
- Test both paths: root daemon (via `DoConnect`) and user daemon (via `forwardConnectToSudo`)

### ConnectionID

- ConnectionID = last 12 hex chars of namespace UID, identifies a VPN connection
- **Generated by user daemon** in `connect_elevate.go` via `util.GetConnectionID()`, stored as `connect.ConnectionID`
- **Transmitted to root daemon** via `ConnectRequest.ConnectionID` proto field
- Root daemon receives and stores it: `connect.ConnectionID = req.ConnectionID`
- Used as primary key for `findConnection`, `removeConnection`, disconnect, status queries
- Design doc: `docs/04-connection-id.md`

## Refactoring Rules

When refactoring backend code (`pkg/`):

1. **Never touch `cmd/`** — CLI is frozen
2. **Always `go build ./...` after changes** — catch compile errors immediately
3. **Run `go test` and `go vet`** on affected packages before committing
4. **Commit per logical change** — don't bundle unrelated fixes
5. **Inherent complexity is OK** — don't split protocol/platform code just to reduce line count
6. **File renames need `git mv`** — preserve git history
7. **Prefer explicit over implicit** — use named fields/types instead of magic value checks
8. **Extract shared helpers** — if 3+ call sites duplicate logic, extract to a shared function
9. **Any modification: verify global impact** —
   - Inserting code in a function: re-read the entire function's control flow
   - Removing a json tag: grep all deserialization paths that read that field
   - Deleting a file/package: grep all `.go` AND `.md` for references
   - Removing all callers of an API: also delete the API implementation
   - Extracting new types: default to unexported, export only when external packages need it
   - Writing a TODO: if it blocks correctness, fix it now; never ship broken code with a TODO
