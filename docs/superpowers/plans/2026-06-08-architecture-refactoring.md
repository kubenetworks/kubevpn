# KubeVPN Architecture Refactoring Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Resolve 6 architectural design issues in KubeVPN's pkg/ layer to improve maintainability, testability, and separation of concerns.

**Architecture:** Incremental refactoring in 6 phases, each producing a compilable, testable intermediate state. Phases ordered by dependency: fix leaf-level issues first (util, rpc coupling), then structural issues (interface boundaries, God Object decomposition), then remaining cleanup (ssh split, rollback unification).

**Tech Stack:** Go 1.23+, gRPC/protobuf, Kubernetes client-go, gvisor networking

---

## Constraints

- **NEVER modify `cmd/`** — CLI layer is frozen
- **Always `go build ./...` after each task** — catch breakage immediately
- **Run `go vet ./pkg/...`** on affected packages
- **Commit per task** — each task is one logical change
- **No new packages without justification** — prefer moving code to existing packages

## Phase Overview

| Phase | Problem | Approach | Risk |
|-------|---------|----------|------|
| 1 | util reverse dependencies | Move grpc.go → daemon, volume.go → run | Low |
| 2 | handler↔rpc coupling | Extract SSH RPC conversion out of pkg/ssh | Low |
| 3 | handler stores rpc.ConnectRequest | Replace with handler-native struct | Medium |
| 4 | daemon-handler no interface | Define Connection interface | Medium |
| 5 | ssh.go mixed responsibilities | Split into tunnel/terminal/installer | Low |
| 6 | ConnectOptions God Object | Extract NetworkManager and ProxyManager | High |

---

## Phase 1: Fix util Reverse Dependencies

**Problem:** `pkg/util/grpc.go` imports `pkg/daemon/rpc`, and `pkg/util/volume.go` imports `pkg/cp`. A leaf utility package should not depend on higher-level packages.

**Strategy:** 
- Move gRPC stream helpers into `pkg/daemon/action/` (all callers are in daemon/action, daemon/client, or run/connect — closer to the transport layer)
- Move volume helpers into `pkg/run/` (sole caller is `pkg/run/options.go`)
- Keep `HandleCrash` in util (used by core/dns — true utility function, no rpc dependency)

### Task 1.1: Extract HandleCrash into its own file

**Files:**
- Create: `pkg/util/crash.go`
- Modify: `pkg/util/grpc.go`

- [ ] **Step 1: Create `pkg/util/crash.go` with HandleCrash**

```go
package util

import (
	"runtime/debug"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func HandleCrash() {
	if r := recover(); r != nil {
		plog.G(context.Background()).Errorf("recovered from panic: %v, stack: %s", r, debug.Stack())
	}
}
```

Note: Copy the exact implementation from `grpc.go:118-127`. The code above is illustrative — use the actual implementation.

- [ ] **Step 2: Remove HandleCrash from `pkg/util/grpc.go`**

Delete the `HandleCrash` function from grpc.go.

- [ ] **Step 3: Verify compilation**

Run: `go build ./...`
Expected: PASS (HandleCrash is still in pkg/util, just a different file)

- [ ] **Step 4: Commit**

```bash
git add pkg/util/crash.go pkg/util/grpc.go
git commit -m "refactor(util): move HandleCrash to its own file"
```

### Task 1.2: Move gRPC stream helpers to pkg/daemon/action

**Files:**
- Delete: `pkg/util/grpc.go`
- Create: `pkg/daemon/action/grpcstream.go`
- Modify: `pkg/daemon/action/connect_elevate.go` (update import)
- Modify: `pkg/daemon/action/sync.go` (update import)
- Modify: `pkg/daemon/action/proxy.go` (update import)
- Modify: `pkg/daemon/action/disconnect.go` (update import)
- Modify: `pkg/daemon/action/connect.go` (update import for ListenCancel)
- Modify: `pkg/daemon/action/persistence.go` (update import)
- Modify: `pkg/daemon/client.go` (update import)
- Modify: `pkg/run/connect.go` (update import)

- [ ] **Step 1: Create `pkg/daemon/action/grpcstream.go`**

Copy all functions from `pkg/util/grpc.go` EXCEPT `HandleCrash` into a new file. Change the package declaration to `package action`. Adjust imports — remove the `pkg/daemon/rpc` import alias since it's now in the same module path. Export the functions that need to be called from `pkg/daemon/client.go` and `pkg/run/connect.go`.

Since `PrintGRPCStream` is called from `pkg/daemon/client.go` and `pkg/run/connect.go` (outside `action`), we have two options:
- Option A: Create `pkg/daemon/grpcutil/` as a shared package
- Option B: Move only `PrintGRPCStream` to `pkg/daemon/` (next to client.go)

Choose **Option A**: Create `pkg/daemon/grpcutil/stream.go` with all gRPC stream functions.

```go
package grpcutil

import (
	"context"
	"io"

	"google.golang.org/grpc"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// Copy all functions from pkg/util/grpc.go except HandleCrash:
// - PrintGRPCStream
// - CopyGRPCStream
// - CopyGRPCConnStream
// - CopyAndConvertGRPCStream
// - ListenCancel
```

- [ ] **Step 2: Update all callers**

Replace `util.PrintGRPCStream` → `grpcutil.PrintGRPCStream` (add import `"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/grpcutil"`):
- `pkg/daemon/action/connect_elevate.go`: `util.CopyGRPCStream` → `grpcutil.CopyGRPCStream`
- `pkg/daemon/action/sync.go`: `util.CopyAndConvertGRPCStream` → `grpcutil.CopyAndConvertGRPCStream`
- `pkg/daemon/action/proxy.go`: `util.CopyGRPCConnStream` → `grpcutil.CopyGRPCConnStream`
- `pkg/daemon/action/disconnect.go`: `util.CopyGRPCStream` → `grpcutil.CopyGRPCStream`
- `pkg/daemon/action/connect.go`: `util.ListenCancel` → `grpcutil.ListenCancel`
- `pkg/daemon/action/persistence.go`: `util.PrintGRPCStream` → `grpcutil.PrintGRPCStream`
- `pkg/daemon/client.go`: `util.PrintGRPCStream` → `grpcutil.PrintGRPCStream`
- `pkg/run/connect.go`: `util.PrintGRPCStream` → `grpcutil.PrintGRPCStream`

- [ ] **Step 3: Delete `pkg/util/grpc.go`**

- [ ] **Step 4: Verify compilation and check util no longer imports daemon/rpc**

Run: `go build ./... && grep -r "daemon/rpc" pkg/util/`
Expected: Build PASS, grep returns nothing.

- [ ] **Step 5: Run vet**

Run: `go vet ./pkg/util/... ./pkg/daemon/...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add -A pkg/util/grpc.go pkg/daemon/grpcutil/ pkg/daemon/action/ pkg/daemon/client.go pkg/run/connect.go
git commit -m "refactor(util): move gRPC stream helpers to pkg/daemon/grpcutil"
```

### Task 1.3: Move volume helpers to pkg/run

**Files:**
- Delete: `pkg/util/volume.go`
- Create: `pkg/run/volume.go`
- Modify: `pkg/run/options.go` (update import)

- [ ] **Step 1: Create `pkg/run/volume.go`**

Copy `GetVolume` and `RemoveDir` from `pkg/util/volume.go`. Change package to `package run`. Adjust the `util.Factory` parameter — since it's now in `pkg/run` which already imports `cmdutil`, use `cmdutil.Factory` directly or keep the reference as needed.

- [ ] **Step 2: Update caller in `pkg/run/options.go`**

Change `util.GetVolume(...)` → `GetVolume(...)` (now same package, no import needed).
Change `util.RemoveDir(...)` → `RemoveDir(...)`.
Remove the `util` import if no longer needed (likely still needed for other util functions).

- [ ] **Step 3: Delete `pkg/util/volume.go`**

- [ ] **Step 4: Verify**

Run: `go build ./... && grep -r "pkg/cp" pkg/util/`
Expected: Build PASS, grep returns nothing.

- [ ] **Step 5: Commit**

```bash
git add -A pkg/util/volume.go pkg/run/volume.go pkg/run/options.go
git commit -m "refactor(util): move volume helpers to pkg/run"
```

---

## Phase 2: Decouple pkg/ssh from daemon/rpc

**Problem:** `pkg/ssh/config.go` imports `pkg/daemon/rpc` for `ParseSshFromRPC` and `ToRPC()`. This couples the SSH library to protobuf definitions.

**Strategy:** Move the RPC conversion functions to the daemon layer. The SSH package should only know about its own `SshConfig` type. Callers that need RPC↔SshConfig conversion use adapter functions in the daemon layer.

### Task 2.1: Move ParseSshFromRPC to daemon/action

**Files:**
- Modify: `pkg/ssh/config.go` (remove ParseSshFromRPC, ToRPC, and rpc import)
- Create: `pkg/daemon/action/sshconv.go` (new conversion functions)
- Modify: `pkg/daemon/action/connect_elevate.go` (update call)
- Modify: `pkg/daemon/action/writer.go` (update call)
- Modify: `pkg/handler/connect.go` (remove ssh.ParseSshFromRPC usage)
- Modify: `pkg/run/connect.go` (update ToRPC call)

- [ ] **Step 1: Create `pkg/daemon/action/sshconv.go`**

```go
package action

import (
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
)

func parseSshFromRPC(sshJump *rpc.SshJump) *ssh.SshConfig {
	if sshJump == nil {
		return &ssh.SshConfig{}
	}
	return &ssh.SshConfig{
		Addr:             sshJump.Addr,
		User:             sshJump.User,
		Password:         sshJump.Password,
		Keyfile:          sshJump.Keyfile,
		ConfigAlias:      sshJump.ConfigAlias,
		GSSAPIKeytabConf: sshJump.GssapiKeytab,
		GSSAPICacheFile:  sshJump.GssapiCache,
		GSSAPIPassword:   sshJump.GssapiPassword,
		RemoteKubeconfig: sshJump.RemoteKubeconfig,
		PortForward:      sshJump.PortForward,
	}
}

func sshConfigToRPC(conf *ssh.SshConfig) *rpc.SshJump {
	if conf == nil {
		return nil
	}
	return &rpc.SshJump{
		Addr:             conf.Addr,
		User:             conf.User,
		Password:         conf.Password,
		Keyfile:          conf.Keyfile,
		ConfigAlias:      conf.ConfigAlias,
		GssapiKeytab:     conf.GSSAPIKeytabConf,
		GssapiCache:      conf.GSSAPICacheFile,
		GssapiPassword:   conf.GSSAPIPassword,
		RemoteKubeconfig: conf.RemoteKubeconfig,
		PortForward:      conf.PortForward,
	}
}
```

Note: Copy exact field mappings from `pkg/ssh/config.go:62-95`. The above is illustrative.

- [ ] **Step 2: Create `pkg/run/sshconv.go` for run package usage**

The `pkg/run/connect.go` calls `sshConfig.ToRPC()`. Since `run` shouldn't import `daemon/action`, create a parallel conversion in `pkg/run/`:

```go
package run

import (
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
)

func sshConfigToRPC(conf *ssh.SshConfig) *rpc.SshJump {
	// same implementation as daemon/action version
}
```

Alternatively, create a shared `pkg/daemon/rpcconv/` package. Choose based on how many callers exist. With only 2 call sites (`daemon/action` and `run`), duplication is acceptable per YAGNI.

- [ ] **Step 3: Update callers**

- `pkg/daemon/action/connect_elevate.go:47`: `ssh.ParseSshFromRPC(req.SshJump)` → `parseSshFromRPC(req.SshJump)`
- `pkg/daemon/action/writer.go:45`: `ssh.ParseSshFromRPC(jump)` → `parseSshFromRPC(jump)`
- `pkg/run/connect.go:57`: `sshConfig.ToRPC()` → `sshConfigToRPC(sshConfig)`
- `pkg/run/connect.go:68`: same

- [ ] **Step 4: Fix handler/connect.go:317**

This line uses `ssh.ParseSshFromRPC(c.Request.SshJump)` to get SSH host IPs. This is part of the larger Phase 3 issue (handler storing `rpc.ConnectRequest`). For now, add a `SshHosts []net.IP` field to ConnectOptions and populate it in daemon/action when creating the struct:

In `pkg/daemon/action/connect_elevate.go`, after creating `connect`:
```go
if sshConf := parseSshFromRPC(req.SshJump); !sshConf.IsEmpty() {
    connect.SshHosts = sshConf.Host()
}
```

In `pkg/handler/connect.go:316-317`, change:
```go
if c.Request != nil {
    c.apiServerIPs = append(c.apiServerIPs, ssh.ParseSshFromRPC(c.Request.SshJump).Host()...)
}
```
To:
```go
if len(c.SshHosts) > 0 {
    c.apiServerIPs = append(c.apiServerIPs, c.SshHosts...)
}
```

Add field to ConnectOptions struct:
```go
SshHosts []net.IP `json:"-"`
```

- [ ] **Step 5: Remove `ParseSshFromRPC` and `ToRPC` from `pkg/ssh/config.go`**

Delete the functions and remove the `"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"` import.

- [ ] **Step 6: Verify**

Run: `go build ./... && grep -r "daemon/rpc" pkg/ssh/`
Expected: Build PASS, grep returns nothing.

- [ ] **Step 7: Run tests**

Run: `go test ./pkg/ssh/... ./pkg/daemon/action/... ./pkg/run/...`
Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add pkg/ssh/config.go pkg/daemon/action/sshconv.go pkg/run/sshconv.go \
        pkg/daemon/action/connect_elevate.go pkg/daemon/action/writer.go \
        pkg/run/connect.go pkg/handler/connect.go
git commit -m "refactor(ssh): decouple pkg/ssh from daemon/rpc types"
```

---

## Phase 3: Remove rpc.ConnectRequest from handler

**Problem:** `ConnectOptions.Request *rpc.ConnectRequest` stores a gRPC message type in the business logic layer. It's used for: (1) persisting config for daemon restart, (2) extracting SSH host IPs (fixed in Phase 2).

**Strategy:** Replace with a handler-native `SessionConfig` struct that contains only what handler needs. The daemon layer converts rpc→SessionConfig when creating ConnectOptions.

### Task 3.1: Define SessionConfig and replace Request field

**Files:**
- Modify: `pkg/handler/connect.go` (replace Request field)
- Create: `pkg/handler/session_config.go` (new type)
- Modify: `pkg/daemon/action/connect.go` (convert rpc→SessionConfig)
- Modify: `pkg/daemon/action/connect_elevate.go` (convert rpc→SessionConfig)
- Modify: `pkg/daemon/action/persistence.go` (adapt LoadFromConfig/OffloadToConfig)

- [ ] **Step 1: Analyze what Request fields are actually used**

From the analysis:
- `persistence.go:88-90`: `c.Request.IPv4`, `c.Request.IPv6` (set from LocalTunIPv4/v6), then `resp.Send(c.Request)` to re-send the original connect request for reload.

So `Request` is stored to replay the connect RPC on daemon restart. The handler doesn't use it for logic — only persistence does.

- [ ] **Step 2: Create `pkg/handler/session_config.go`**

```go
package handler

// SessionRequest stores the serialized original connect parameters for daemon
// restart recovery. The handler layer treats this as opaque bytes — only the
// daemon persistence layer reads/writes it.
type SessionRequest struct {
	Raw []byte `json:"Raw,omitempty"`
}
```

- [ ] **Step 3: Replace Request field in ConnectOptions**

In `pkg/handler/connect.go`, change:
```go
Request *rpc.ConnectRequest `json:"Request,omitempty"`
```
To:
```go
SessionRequest *SessionRequest `json:"SessionRequest,omitempty"`
```

Remove the `"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"` import from `connect.go`.

- [ ] **Step 4: Update daemon/action to serialize Request into SessionRequest**

In `pkg/daemon/action/connect.go` and `connect_elevate.go`, after creating `connect`:
```go
rawReq, _ := proto.Marshal(req)
connect.SessionRequest = &handler.SessionRequest{Raw: rawReq}
```

- [ ] **Step 5: Update persistence.go to use SessionRequest**

In `LoadFromConfig`, after unmarshalling a ConnectOptions, reconstruct the rpc.ConnectRequest from `SessionRequest.Raw`:
```go
var req rpc.ConnectRequest
if err := proto.Unmarshal(c.SessionRequest.Raw, &req); err != nil {
    continue
}
req.IPv4 = c.LocalTunIPv4.String()
req.IPv6 = c.LocalTunIPv6.String()
err = resp.Send(&req)
```

- [ ] **Step 6: Verify**

Run: `go build ./... && grep -r "daemon/rpc" pkg/handler/`
Expected: Build PASS, grep returns nothing (only rpc reference should now be gone from handler).

- [ ] **Step 7: Run tests**

Run: `go test ./pkg/handler/... ./pkg/daemon/action/...`
Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add pkg/handler/connect.go pkg/handler/session_config.go \
        pkg/daemon/action/connect.go pkg/daemon/action/connect_elevate.go \
        pkg/daemon/action/persistence.go
git commit -m "refactor(handler): replace rpc.ConnectRequest with opaque SessionRequest"
```

---

## Phase 4: Define Connection Interface

**Problem:** `daemon/action/Server` stores `[]*handler.ConnectOptions` and directly accesses 15+ methods/fields. No abstraction boundary exists between daemon orchestration and connection business logic.

**Strategy:** Define a `Connection` interface in `pkg/handler` exposing only what daemon/action needs. Server stores `[]Connection`. This decouples daemon from ConnectOptions internals.

### Task 4.1: Identify the daemon→handler API surface

**Files:**
- Read: `pkg/daemon/action/` (all files using handler.ConnectOptions)

From the analysis, daemon/action uses these ConnectOptions methods/fields:

**Methods called:**
- `InitClient(f)` — init K8s client
- `InitDHCP(ctx)` — init DHCP
- `RentIP(ctx, ipv4, ipv6)` — lease IPs
- `GetIPFromContext(ctx, logger)` — extract IPs from gRPC metadata
- `DoConnect(ctx)` — run full connection
- `Cleanup(ctx)` — teardown
- `GetConnectionID()` — ID for lookup
- `GetLocalTunIP()` — (v4, v6 strings)
- `HealthCheckOnce(ctx, timeout)` — health check
- `HealthPeriod(ctx, duration)` — periodic health
- `HealthStatus()` — last health
- `AddRollbackFunc(f)` — add cleanup
- `LeaveResource(ctx, resources, v4)` — leave proxy
- `CreateRemoteInboundPod(ctx, ns, workloads, headers, portMap, image)` — proxy
- `ProxyResources()` — list proxied resources
- `IsMe(ns, uid, headers)` — ownership check
- `Context()` — session context

**Fields accessed directly:**
- `ManagerNamespace` — read/write
- `ExtraRouteInfo` — read/write
- `OriginKubeconfigPath` — read/write
- `OriginNamespace` — read
- `Lock` — shared mutex
- `Image` — read
- `ImagePullSecretName` — read
- `LocalTunIPv4` / `LocalTunIPv6` — read
- `Request` / `SessionRequest` — read/write
- `OwnerID` — read/write
- `Sync` — read (SyncOptions attached)
- `K8sClient` (embedded) — `GetFactory()`, `GetClientset()`

- [ ] **Step 1: Define Connection interface in `pkg/handler/connection.go`**

```go
package handler

import (
	"context"
	"net"
	"time"

	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

// Connection defines the interface that the daemon layer uses to interact with
// a VPN connection session. This decouples daemon orchestration from the
// ConnectOptions implementation details.
type Connection interface {
	// Lifecycle
	InitClient(f cmdutil.Factory) error
	DoConnect(ctx context.Context) error
	Cleanup(ctx context.Context)
	Context() context.Context
	AddRollbackFunc(f func() error)

	// Identity
	GetConnectionID() string
	GetLocalTunIP() (v4, v6 string)
	IsMe(ns, uid string, headers map[string]string) bool

	// DHCP
	InitDHCP(ctx context.Context) error
	RentIP(ctx context.Context, ipv4, ipv6 string) (context.Context, error)
	GetIPFromContext(ctx context.Context, logger interface{}) error

	// Health
	HealthCheckOnce(ctx context.Context, timeout time.Duration)
	HealthPeriod(ctx context.Context, interval time.Duration)
	HealthStatus() HealthStatus

	// Proxy
	CreateRemoteInboundPod(ctx context.Context, namespace string, workloads []string, headers map[string]string, portMap []string, image string) error
	LeaveResource(ctx context.Context, resources []Resources, v4 string) error
	ProxyResources() ProxyList

	// Accessors for daemon persistence (will shrink as persistence is refactored)
	GetManagerNamespace() string
	GetSync() *SyncOptions
	GetFactory() cmdutil.Factory
	GetClientset() interface{} // kubernetes.Interface
}
```

Note: This is the target interface. It will be implemented incrementally — first define it, then migrate daemon/action callers one by one.

- [ ] **Step 2: Add accessor methods to ConnectOptions for fields accessed directly**

Add to `pkg/handler/connect.go`:
```go
func (c *ConnectOptions) GetManagerNamespace() string { return c.ManagerNamespace }
func (c *ConnectOptions) GetSync() *SyncOptions       { return c.Sync }
```

- [ ] **Step 3: Verify ConnectOptions satisfies the interface**

Add a compile-time check:
```go
var _ Connection = (*ConnectOptions)(nil)
```

Run: `go build ./pkg/handler/...`
Expected: PASS (if it fails, add missing methods)

- [ ] **Step 4: Commit (interface definition only)**

```bash
git add pkg/handler/connection.go pkg/handler/connect.go
git commit -m "refactor(handler): define Connection interface for daemon boundary"
```

### Task 4.2: Migrate Server.connections to use Connection interface

**Files:**
- Modify: `pkg/daemon/action/persistence.go` (change slice type)
- Modify: `pkg/daemon/action/connection.go` (update findConnection)
- Modify: `pkg/daemon/action/status.go` (update type assertions)
- Modify: `pkg/daemon/action/disconnect.go`
- Modify: `pkg/daemon/action/proxy.go`

- [ ] **Step 1: Change `connections` field type**

In `pkg/daemon/action/persistence.go`:
```go
connections []*handler.ConnectOptions
```
→
```go
connections []handler.Connection
```

- [ ] **Step 2: Update findConnection return type**

In `pkg/daemon/action/connection.go`:
```go
func (svr *Server) findConnection(connectionID string) (handler.Connection, int) {
```

- [ ] **Step 3: Update status.go to use interface methods**

Replace direct field access with interface method calls. Where type assertion is needed for persistence-specific operations (like `SessionRequest`), use:
```go
if co, ok := conn.(*handler.ConnectOptions); ok {
    // persistence-specific access
}
```

- [ ] **Step 4: Update all other daemon/action files that access connections**

Each file that iterates `svr.connections` or calls methods on connection objects should work through the `Connection` interface.

- [ ] **Step 5: Verify**

Run: `go build ./... && go test ./pkg/daemon/action/...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add pkg/daemon/action/
git commit -m "refactor(daemon/action): use Connection interface for connection management"
```

---

## Phase 5: Split daemon/handler/ssh.go

**Problem:** 434 lines mixing WebSocket HTTP handling, SSH tunneling, remote daemon installation, and session registry management.

**Strategy:** Split into 3 focused files by responsibility.

### Task 5.1: Extract session registry

**Files:**
- Create: `pkg/daemon/handler/registry.go`
- Modify: `pkg/daemon/handler/ssh.go`

- [ ] **Step 1: Create `pkg/daemon/handler/registry.go`**

Move these from ssh.go:
- `type registry struct` and all its methods (`storeSession`, `loadSession`, `storeReady`, `loadReady`, `cleanup`)
- `var sessionRegistry`
- The `init()` function (if it only initializes the registry — check first)

- [ ] **Step 2: Remove moved code from ssh.go**

- [ ] **Step 3: Verify**

Run: `go build ./pkg/daemon/handler/...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add pkg/daemon/handler/registry.go pkg/daemon/handler/ssh.go
git commit -m "refactor(daemon/handler): extract session registry from ssh.go"
```

### Task 5.2: Extract remote installer

**Files:**
- Create: `pkg/daemon/handler/installer.go`
- Modify: `pkg/daemon/handler/ssh.go`

- [ ] **Step 1: Create `pkg/daemon/handler/installer.go`**

Move from ssh.go:
- `func (w *wsHandler) installKubevpnOnRemote(ctx, sshClient)` (and any helper it calls exclusively)
- `func startDaemonProcess(cli)` 
- `func parseDaemonVersion(output)`

- [ ] **Step 2: Remove moved code from ssh.go**

- [ ] **Step 3: Verify**

Run: `go build ./pkg/daemon/handler/...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add pkg/daemon/handler/installer.go pkg/daemon/handler/ssh.go
git commit -m "refactor(daemon/handler): extract remote installer from ssh.go"
```

### Task 5.3: Extract tunnel logic

**Files:**
- Create: `pkg/daemon/handler/tunnel.go`
- Modify: `pkg/daemon/handler/ssh.go`

- [ ] **Step 1: Create `pkg/daemon/handler/tunnel.go`**

Move from ssh.go:
- `func (w *wsHandler) createTunnel(ctx, cli)` 
- `type connWatcher` and its methods

- [ ] **Step 2: Remove moved code from ssh.go**

ssh.go should now only contain:
- `type wsHandler` struct definition
- `func (w *wsHandler) handle(lite bool)` — the main dispatch
- `func (w *wsHandler) terminal(ctx, cli, conn)` — terminal session
- `func (w *wsHandler) log(format, a)` — logging helper

- [ ] **Step 3: Verify**

Run: `go build ./pkg/daemon/handler/... && go vet ./pkg/daemon/handler/...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add pkg/daemon/handler/tunnel.go pkg/daemon/handler/ssh.go
git commit -m "refactor(daemon/handler): extract tunnel logic from ssh.go"
```

---

## Phase 6: Decompose ConnectOptions (God Object)

**Problem:** ConnectOptions has 30+ fields and 42 methods across 10 files, handling DHCP, TUN, routing, DNS, health checking, proxy management, and cleanup.

**Strategy:** Extract two focused managers that own distinct subsets of state:
1. `NetworkManager` — TUN device, routes, DNS, port-forward (data-plane concerns)
2. `ProxyManager` — proxy workload lifecycle, leave/reset

ConnectOptions becomes the session orchestrator that creates and coordinates these managers.

**Risk:** This is the highest-risk phase. Each sub-task must leave the code compilable.

### Task 6.1: Extract NetworkManager (TUN + Routes + DNS)

**Files:**
- Create: `pkg/handler/network.go`
- Modify: `pkg/handler/connect_tun.go` (move TUN methods)
- Modify: `pkg/handler/connect_route.go` (move route methods)
- Modify: `pkg/handler/connect_dns.go` (move DNS methods)
- Modify: `pkg/handler/connect.go` (delegate to NetworkManager)

- [ ] **Step 1: Define NetworkManager struct**

Create `pkg/handler/network.go`:
```go
package handler

import (
	"context"
	"net"
	"sync"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
)

// NetworkManager owns TUN device setup, route table management, DNS configuration,
// and port-forwarding to the traffic manager pod. It encapsulates the data-plane
// networking concerns that were previously spread across ConnectOptions.
type NetworkManager struct {
	clientset kubernetes.Interface
	config    *rest.Config
	namespace string

	tunName      string
	cidrs        []*net.IPNet
	apiServerIPs []net.IP
	extraHost    []dns.Entry
	dnsConfig    *dns.Config

	localTunIPv4 *net.IPNet
	localTunIPv6 *net.IPNet

	svcInformer cache.SharedIndexInformer
	podInformer cache.SharedIndexInformer

	mu sync.Mutex
}

func NewNetworkManager(clientset kubernetes.Interface, config *rest.Config, namespace string) *NetworkManager {
	return &NetworkManager{
		clientset: clientset,
		config:    config,
		namespace: namespace,
	}
}
```

- [ ] **Step 2: Move route methods to NetworkManager**

Move from ConnectOptions to NetworkManager (change receiver):
- `addRouteDynamic` → `(nm *NetworkManager) AddRouteDynamic`
- `watchAndRoute` → `(nm *NetworkManager) watchAndRoute`
- `addRoute` → `(nm *NetworkManager) AddRoute`
- `addExtraRoute` → `(nm *NetworkManager) AddExtraRoute`
- `addExtraNodeIP` → `(nm *NetworkManager) AddExtraNodeIP`

Update `connect_route.go` to use NetworkManager receiver. The `ExtraRouteInfo` is passed as parameter rather than accessed via struct field.

- [ ] **Step 3: Update ConnectOptions to delegate**

Add a `network *NetworkManager` field to ConnectOptions. In `DoConnect`:
```go
c.network = NewNetworkManager(c.clientset, c.config, c.ManagerNamespace)
c.network.localTunIPv4 = c.LocalTunIPv4
c.network.localTunIPv6 = c.LocalTunIPv6
// ... delegate route/dns calls to c.network
```

- [ ] **Step 4: Move TUN and port-forward methods**

Move to NetworkManager:
- `portForward` → `(nm *NetworkManager) PortForward`
- `portForwardOnce` → `(nm *NetworkManager) portForwardOnce`
- `startLocalTunServer` → `(nm *NetworkManager) StartLocalTunServer`

- [ ] **Step 5: Move DNS methods**

Move to NetworkManager:
- `setupDNS` → `(nm *NetworkManager) SetupDNS`

- [ ] **Step 6: Update DoConnect to use NetworkManager**

```go
func (c *ConnectOptions) DoConnect(ctx context.Context) (err error) {
    c.ctx, c.cancel = context.WithCancel(ctx)
    c.isDataPlane = true
    // ... DHCP init, getCIDR ...
    
    c.network = NewNetworkManager(c.clientset, c.config, c.ManagerNamespace)
    c.network.SetTunIPs(c.LocalTunIPv4, c.LocalTunIPv6)
    c.network.SetCIDRs(c.cidrs)
    c.network.SetAPIServerIPs(c.apiServerIPs)
    
    if err = c.network.PortForward(ctx, portPair); err != nil {
        return
    }
    if err = c.network.StartLocalTunServer(ctx, forward); err != nil {
        return
    }
    if err = c.network.AddRouteDynamic(ctx, c.OriginNamespace, c.ExtraRouteInfo); err != nil {
        return
    }
    if err = c.network.SetupDNS(ctx); err != nil {
        return
    }
    return
}
```

- [ ] **Step 7: Verify**

Run: `go build ./... && go test ./pkg/handler/...`
Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add pkg/handler/network.go pkg/handler/connect.go \
        pkg/handler/connect_tun.go pkg/handler/connect_route.go \
        pkg/handler/connect_dns.go
git commit -m "refactor(handler): extract NetworkManager from ConnectOptions"
```

### Task 6.2: Extract ProxyManager

**Files:**
- Create: `pkg/handler/proxy_manager.go`
- Modify: `pkg/handler/proxy.go` (move ProxyList methods)
- Modify: `pkg/handler/leave.go` (move to ProxyManager)
- Modify: `pkg/handler/connect.go` (delegate proxy operations)

- [ ] **Step 1: Define ProxyManager struct**

Create `pkg/handler/proxy_manager.go`:
```go
package handler

import (
	"context"
	"sync"

	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

// ProxyManager handles the lifecycle of proxy workloads (sidecar injection,
// leave/unpatch). It owns the list of currently proxied resources and provides
// methods to add, remove, and query proxy state.
type ProxyManager struct {
	factory   cmdutil.Factory
	namespace string
	ownerID   string

	mu        sync.Mutex
	workloads ProxyList
}

func NewProxyManager(factory cmdutil.Factory, namespace, ownerID string) *ProxyManager {
	return &ProxyManager{
		factory:   factory,
		namespace: namespace,
		ownerID:   ownerID,
	}
}

func (pm *ProxyManager) Add(proxy *Proxy) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.workloads.Add(proxy)
}

func (pm *ProxyManager) Remove(ns, workload string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.workloads.Remove(ns, workload)
}

func (pm *ProxyManager) Resources() ProxyList {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	result := make(ProxyList, len(pm.workloads))
	copy(result, pm.workloads)
	return result
}

func (pm *ProxyManager) IsMe(ns, uid string, headers map[string]string) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.workloads.IsMe(ns, uid, headers)
}
```

- [ ] **Step 2: Move LeaveAllProxyResources and LeaveResource**

Move from ConnectOptions to ProxyManager:
- `LeaveAllProxyResources` → `(pm *ProxyManager) LeaveAll`
- `LeaveResource` → `(pm *ProxyManager) Leave`

- [ ] **Step 3: Add proxyManager field to ConnectOptions and delegate**

```go
type ConnectOptions struct {
    // ...
    proxyManager *ProxyManager
}

func (c *ConnectOptions) ProxyResources() ProxyList {
    if c.proxyManager == nil {
        return nil
    }
    return c.proxyManager.Resources()
}
```

- [ ] **Step 4: Verify**

Run: `go build ./... && go test ./pkg/handler/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add pkg/handler/proxy_manager.go pkg/handler/proxy.go \
        pkg/handler/leave.go pkg/handler/connect.go
git commit -m "refactor(handler): extract ProxyManager from ConnectOptions"
```

### Task 6.3: Unify rollback into SessionLifecycle

**Files:**
- Modify: `pkg/handler/connect.go` (replace rollbackFuncList with SessionLifecycle)
- Modify: `pkg/handler/cleaner.go` (use SessionLifecycle for cleanup)
- Modify: `pkg/daemon/action/lifecycle.go` (verify API compatibility)

- [ ] **Step 1: Add SessionLifecycle to ConnectOptions**

Replace the raw `rollbackFuncList []func() error` with a `*SessionLifecycle` field (or embed the lifecycle pattern):

```go
type ConnectOptions struct {
    // ...
    lifecycle *Lifecycle // replaces rollbackFuncList
}

// Lifecycle manages LIFO cleanup functions for a connection session.
type Lifecycle struct {
    mu    sync.Mutex
    funcs []func() error
}

func (l *Lifecycle) AddCleanup(f func() error) {
    l.mu.Lock()
    defer l.mu.Unlock()
    l.funcs = append(l.funcs, f)
}

func (l *Lifecycle) RunCleanups(ctx context.Context) {
    l.mu.Lock()
    funcs := l.funcs
    l.funcs = nil
    l.mu.Unlock()
    // Run in reverse (LIFO)
    for i := len(funcs) - 1; i >= 0; i-- {
        if funcs[i] != nil {
            if err := funcs[i](); err != nil {
                plog.G(ctx).Warnf("Cleanup error: %v", err)
            }
        }
    }
}
```

- [ ] **Step 2: Replace AddRollbackFunc with lifecycle.AddCleanup**

Update all callers of `c.AddRollbackFunc(...)` to use `c.lifecycle.AddCleanup(...)`.

- [ ] **Step 3: Update Cleanup to use lifecycle**

In `cleaner.go`, change `executeRollbackFuncs(logCtx, c.getRollbackFuncs())` to `c.lifecycle.RunCleanups(logCtx)`.

- [ ] **Step 4: Verify**

Run: `go build ./... && go test ./pkg/handler/... ./pkg/daemon/action/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add pkg/handler/connect.go pkg/handler/cleaner.go pkg/handler/lifecycle.go
git commit -m "refactor(handler): unify rollback mechanism with Lifecycle type"
```

---

## Verification Checklist

After all phases complete:

- [ ] `go build ./...` — passes
- [ ] `go vet ./pkg/...` — passes
- [ ] `go test ./pkg/...` — passes (excluding TestPing)
- [ ] `grep -r "daemon/rpc" pkg/util/` — no results
- [ ] `grep -r "daemon/rpc" pkg/ssh/` — no results
- [ ] `grep -r "daemon/rpc" pkg/handler/` — no results
- [ ] ConnectOptions method count < 25 (down from 42)
- [ ] No file in pkg/handler/ exceeds 300 lines (excluding generated)

## Dependency Graph After Refactoring

```
cmd/ (frozen)
 └── pkg/daemon/action/   → pkg/handler (via Connection interface)
      └── pkg/daemon/grpcutil/  → pkg/daemon/rpc
 └── pkg/handler/         → pkg/config, pkg/util, pkg/core, pkg/inject, pkg/dns, pkg/tun, pkg/dhcp
 └── pkg/ssh/             → pkg/config, pkg/util (NO rpc dependency)
 └── pkg/util/            → pkg/config (NO upward dependencies)
 └── pkg/core/            → pkg/config, pkg/util, pkg/tun
```
