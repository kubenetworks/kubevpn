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
│   ├── handler/       WebSocket SSH terminal handler
│   ├── elevate/       Privilege escalation (sudo/admin)
│   └── rpc/           Generated protobuf (DO NOT EDIT *.pb.go)
├── dhcp/              DHCP IP allocation via ConfigMap
├── dns/               DNS setup (platform-specific: linux/unix/windows)
├── driver/            TUN/TAP driver management (wintun, openvpn)
├── handler/           Core business logic
│   ├── connect.go         ConnectOptions struct + DoConnect orchestration
│   ├── connect_tun.go     TUN server, port forwarding, health checks
│   ├── connect_route.go   Dynamic routing, extra routes
│   ├── connect_dns.go     DNS setup
│   ├── connect_upgrade.go Traffic manager deployment upgrade
│   ├── traffmgr.go        Create traffic manager pod
│   ├── traffmgr_resources.go  K8s resource generators (deploy, svc, secret, etc.)
│   ├── leave.go           Leave/unpatch proxy resources
│   ├── proxy.go           Port mapping management
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
- **PodContext bundle** in `pkg/run` — groups K8s-fetched data (template, env, volume, DNS)
- **Registry pattern** in `pkg/daemon/handler` — `sync.Map`-backed session registry

### Shared Helpers (avoid re-implementing these)

| Helper | Package | Purpose |
|---|---|---|
| `newStreamWriter(send)` | `daemon/action` | Adapts gRPC streaming Send into `io.Writer` — do NOT create per-action wrapper structs |
| `resolveKubeconfig(ctx, jump, bytes, portForward)` | `daemon/action` | SSH jump + kubeconfig file resolution — do NOT inline the SSH/kubeconfig pattern |
| `util.InitKubeClient(f)` | `pkg/util` | Returns (config, restclient, clientset, namespace) — used by `InitClient` methods |
| `gatherContainerPorts(spec, portMaps)` | `pkg/inject` | Collects container ports from pod spec + portMaps — shared by mesh and fargate |
| `svr.findConnection(id)` | `daemon/action` | Finds connection by ID — do NOT write `for range svr.connections` lookup loops |
| `svr.removeConnection(id)` | `daemon/action` | Removes connections by ID from slice, returns them for caller cleanup |
| `svr.resetCurrentConnection(id)` | `daemon/action` | After removing a connection, picks first remaining as current |

### Fargate/Service Mode

- Detection: use `virtual.IsFargateMode()` which checks the explicit `FargateMode` bool field first
- Do NOT infer mode from `EnvoyListenerPort != 0` — that is a legacy fallback only
- Port names: use `config.PortNameTCP`, `config.PortNameEnvoy`, `config.PortNameHTTP`, `config.PortNameDNS`
- Rollback functions: always `AddRollbackFunc` (not `AddRolloutFunc`)
- Design doc: `docs/fargate-mode.md`

### ConnectOptions Dual-Role Pattern (IMPORTANT)

`ConnectOptions` is used in BOTH daemon layers with different initialization:

| | User Daemon (control plane) | Sudo Daemon (data plane) |
|---|---|---|
| Init path | `InitClient` + `InitDHCP` | `InitClient` + `DoConnect` |
| `c.ctx` | **nil** — never set | Set by `DoConnect` |
| `c.cancel` | **nil** | Set by `DoConnect` |
| Role | DHCP lease, proxy inject, health check relay | TUN device, port-forward, DNS, routes |

**Rules when modifying `ConnectOptions`:**
- `Cleanup` uses `c.ctx != nil` to distinguish roles — do NOT set `c.ctx` in user daemon path
- New methods shared by both paths must NOT assume `c.ctx` is set
- Resources with lifecycle (informers, watchers) must have their own stop channel, not piggyback on `c.ctx`
- Test both paths: sudo daemon (via `DoConnect`) and user daemon (via `forwardConnectToSudo`)

### ConnectionID

- ConnectionID = last 12 hex chars of namespace UID, identifies a VPN connection
- Generated in `dhcp.Manager.InitDHCP()`, cached and exposed via `GetConnectionID()`
- Used as primary key for `findConnection`, `removeConnection`, disconnect, status queries
- Design doc: `docs/connection-id.md`

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
