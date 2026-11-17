# AI Agent Access Implementation Plan (docs/56)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship docs/56 P1 (agent-facing usage guide) + P2 (`kubevpn mcp` subcommand — a stdio MCP server that thin-wraps the user daemon's gRPC API).

**Architecture:** `pkg/mcp` holds all implementation: a `daemonAPI` interface (satisfied by generated `rpc.DaemonClient`, faked in tests), typed tool handlers tested as plain functions, and an MCP server assembly. `cmd/kubevpn/cmds/mcp.go` is a thin wiring layer — `cmd/` is frozen for *existing* commands; a new subcommand is allowed. Sessions live daemon-side: the `Connect`/`Proxy`/`Leave`/`Disconnect` streams close with EOF once the operation completes (verified: `cmd/kubevpn/cmds/progress.go:46` — CLI also just drains to EOF and returns while the daemon keeps the session), so tools need no session registry.

**Tech Stack:** Go 1.26.3, `github.com/modelcontextprotocol/go-sdk/mcp` (new, only new dependency), `google.golang.org/grpc` generic streaming clients, cobra.

## Global Constraints

- Module: `github.com/wencaiwulue/kubevpn/v2`; build with `CGO_ENABLED=0` (no `-race`).
- `cmd/` frozen: existing commands' parameters/behavior/output must not change. Only NEW file `cmd/kubevpn/cmds/mcp.go` + one registration line in `cmd/kubevpn/cmds/root.go`.
- Generated files off-limits: `pkg/daemon/rpc/*.pb.go`.
- Logging: `plog.G(ctx)` with alias `plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"`. Errors: lowercase, wrap with `%w`.
- Non-`integration` tests must run with NO kubeconfig — never point `genericclioptions.ConfigFlags` at the ambient default.
- Verification loop before every commit: `go build ./...` → `go vet ./pkg/...` → `go test ./pkg/...`.
- Naming: abbreviations all-caps (`ID` not `Id`); `Kubeconfig` one word.

---

### Task 1: P1 — Agent-facing usage guide

**Files:**
- Create: `docs/agents-guide.md`

**Interfaces:**
- Consumes: nothing (docs only).
- Produces: the canonical agent workflow referenced by `pkg/mcp` tool descriptions (Task 4–6) and by docs/56 §3.

- [ ] **Step 1: Write the guide**

Create `docs/agents-guide.md` with exactly this structure and content (adapted to the verified facts: `status --json` exists via `--output` flag on status; exit codes in docs/31; elevation is interactive):

```markdown
# Using KubeVPN with AI Coding Agents

KubeVPN gives a local process (yours, your debugger's, or an AI agent's) access to a
Kubernetes cluster network, with optional header-scoped traffic interception. This page
is the canonical workflow for AI agents (Claude Code, Cursor, Codex, Copilot, Windsurf)
driving KubeVPN unattended.

## Canonical workflow

1. `kubevpn status --output json` — check whether a connection already exists. If one is
   Connected, reuse it; do not connect again.
2. `kubevpn connect --namespace <ns>` — establish the VPN. Wait for the command to exit
   (it returns once the session is established; the daemon owns the session).
3. `kubevpn proxy --namespace <ns> --headers X-KubeVPN-User=<agent-id> <workload>` —
   intercept the target workload. ALWAYS scope with `--headers`; a bare `proxy` (no
   headers) routes ALL traffic on declared ports to your machine.
4. Verify: request the workload through cluster DNS or service IP from the local process.
5. `kubevpn leave --namespace <ns> <workload>` — restore the workload before finishing.
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
```

- [ ] **Step 2: Verify referenced files exist**

Run: `ls docs/31-exit-codes.md && grep -c "headers" docs/agents-guide.md`
Expected: file listed; count ≥ 3.

- [ ] **Step 3: Commit**

```bash
git add docs/agents-guide.md
git commit -m "docs: add AI coding agent usage guide (docs/56 P1)"
```

---

### Task 2: MCP SDK dependency + `pkg/mcp` server skeleton

**Files:**
- Modify: `go.mod`, `go.sum`
- Create: `pkg/mcp/server.go`

**Interfaces:**
- Consumes: `pkg/config.Version`.
- Produces: `func Run(ctx context.Context, opts Options) error` and `type Options` — every later task and `cmd/kubevpn/cmds/mcp.go` (Task 7) depend on these exact signatures.

- [ ] **Step 1: Add the dependency**

Run: `go get github.com/modelcontextprotocol/go-sdk@latest && go mod tidy`
Expected: `go.mod` gains `github.com/modelcontextprotocol/go-sdk` with no other dependency changes beyond its transitive requirements.

- [ ] **Step 2: Write the server skeleton**

Create `pkg/mcp/server.go`:

```go
// Package mcp implements the `kubevpn mcp` stdio MCP server. It is a thin adapter
// over the user daemon's gRPC API (pkg/daemon): tool handlers hold no business
// logic, sessions live daemon-side, and destructive operations (Uninstall, Reset,
// Upgrade, Quit) are deliberately not exposed.
package mcp

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Options carries everything the command layer resolves once at startup.
type Options struct {
	// KubeconfigBytes is the resolved kubeconfig (util.ConvertToKubeConfigBytes),
	// sent as ConnectRequest.KubeconfigBytes / ProxyRequest.KubeconfigBytes.
	KubeconfigBytes []byte
	// Namespace is the default namespace from the resolved kubeconfig.
	Namespace string
	// OriginKubeconfigPath is the resolved kubeconfig path
	// (util.GetKubeConfigPath), sent as OriginKubeconfigPath.
	OriginKubeconfigPath string
	// Version is the client version reported by the kubevpn_version tool.
	Version string
}

// Run serves MCP over stdio until ctx is cancelled or stdin closes.
func Run(ctx context.Context, opts Options) error {
	server := mcp.NewServer(&mcp.Implementation{Name: "kubevpn", Version: opts.Version}, nil)
	registerTools(server, opts)
	return server.Run(ctx, &mcp.StdioTransport{})
}
```

- [ ] **Step 3: Make it compile with a temporary registration stub**

Create `pkg/mcp/tools.go`:

```go
package mcp

import "github.com/modelcontextprotocol/go-sdk/mcp"

// registerTools wires every MCP tool to its handler. Tools are added per-feature
// in tools_status.go / tools_connect.go / tools_proxy.go.
func registerTools(server *mcp.Server, opts Options) {
	// filled in by Tasks 4-6
}
```

- [ ] **Step 4: Verify build**

Run: `go build ./...`
Expected: PASS. If `server.Run` or the `Annotations` field names differ in the SDK version resolved by `go get`, adjust mechanically to the SDK's actual API (same semantics; consult `go doc github.com/modelcontextprotocol/go-sdk/mcp Server.Run`).

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum pkg/mcp/
git commit -m "feat(mcp): add MCP SDK and pkg/mcp server skeleton"
```

---

### Task 3: `daemonAPI` adapter + progress drain helper + fake

**Files:**
- Create: `pkg/mcp/daemon.go`
- Test: `pkg/mcp/daemon_test.go`

**Interfaces:**
- Consumes: generated `rpc.DaemonClient` (NOT modified), `grpc.BidiStreamingClient[T, R]`.
- Produces:
  - `type daemonAPI interface { ... }` (exact methods below) — all handlers take this;
  - `var _ daemonAPI = rpc.DaemonClient(nil)` compile assertion;
  - `func drainProgress[T interface{ GetMessage() string }](s progressStream[T]) (last string, msgs []string, err error)`;
  - `type progressStream[T any] interface { RecvMsg(msg T) error }`;
  - `var errElevationRequired` sentinel + `func withHint(err error) error` — handlers wrap daemon failures with agent-actionable hints.

- [ ] **Step 1: Write the failing test**

Create `pkg/mcp/daemon_test.go`:

```go
package mcp

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// fakeStream replays pre-set messages then EOF (or err).
type fakeStream struct {
	msgs   []string
	err    error
	called int
}

func (f *fakeStream) RecvMsg(m msgWithMessage) error {
	if f.called < len(f.msgs) {
		m.Message = f.msgs[f.called]
		f.called++
		return nil
	}
	if f.err != nil {
		return f.err
	}
	return io.EOF
}

type msgWithMessage struct{ Message string }

func (m msgWithMessage) GetMessage() string { return m.Message }

func TestDrainProgressReturnsMessagesThenEOF(t *testing.T) {
	s := &fakeStream{msgs: []string{"step 1", "step 2"}}
	last, msgs, err := drainProgress[msgWithMessage](s)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("want io.EOF on success, got %v", err)
	}
	if last != "step 2" || len(msgs) != 2 {
		t.Fatalf("got last=%q msgs=%v, want last=step 2, 2 messages", last, msgs)
	}
}

func TestDrainProgressPropagatesError(t *testing.T) {
	sentinel := errors.New("boom")
	_, _, err := drainProgress[msgWithMessage](&fakeStream{err: sentinel})
	if !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel, got %v", err)
	}
}

func TestDaemonClientSatisfiesInterface(t *testing.T) {
	var _ daemonAPI = rpc.DaemonClient(nil)
}
```

Note: this test file uses a local `msgWithMessage` type satisfying `GetMessage() string` so it needs no RPC types and no kubeconfig.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./pkg/mcp/ -run TestDrainProgress -v`
Expected: FAIL — `undefined: drainProgress`, `undefined: daemonAPI`.

- [ ] **Step 3: Write the implementation**

Create `pkg/mcp/daemon.go`:

```go
package mcp

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// daemonAPI is the slice of the daemon gRPC surface the MCP tools expose. It
// exists so handlers are testable against a fake; rpc.DaemonClient satisfies it.
type daemonAPI interface {
	Version(ctx context.Context, in *rpc.VersionRequest) (*rpc.VersionResponse, error)
	Status(ctx context.Context, in *rpc.StatusRequest) (*rpc.StatusResponse, error)
	ConnectionList(ctx context.Context, in *rpc.ConnectionListRequest) (*rpc.ConnectionListResponse, error)
	Connect(ctx context.Context) (grpc.BidiStreamingClient[rpc.ConnectRequest, rpc.ConnectResponse], error)
	Proxy(ctx context.Context) (grpc.BidiStreamingClient[rpc.ProxyRequest, rpc.ProxyResponse], error)
	Leave(ctx context.Context) (grpc.BidiStreamingClient[rpc.LeaveRequest, rpc.LeaveResponse], error)
	Disconnect(ctx context.Context) (grpc.BidiStreamingClient[rpc.DisconnectRequest, rpc.DisconnectResponse], error)
	Logs(ctx context.Context, in *rpc.LogRequest) (rpc.Daemon_LogsClient, error)
}

var _ daemonAPI = rpc.DaemonClient(nil)

// progressStream is the read side of a streaming RPC call.
type progressStream[T any] interface {
	RecvMsg(msg T) error
}

// drainProgress consumes a streaming RPC until terminal state, returning the
// last message, every message, and the terminal error — io.EOF on success.
// Mirrors cmds/progress.go: the daemon closes the stream when the operation
// completes; sessions live daemon-side.
func drainProgress[T interface{ GetMessage() string }](s progressStream[T]) (string, []string, error) {
	var last string
	var msgs []string
	for {
		var m T
		if err := s.RecvMsg(&m); err != nil {
			return last, msgs, err // io.EOF on success; real errors propagate
		}
		msgs = append(msgs, m.GetMessage())
		last = m.GetMessage()
	}
}
```

Append to `pkg/mcp/daemon.go`:

```go
// elevationHint is the handoff instruction for the interactive sudo prompt,
// per docs/agents-guide.md.
const elevationHint = "the root daemon requires an interactive sudo prompt: " +
	"surface the command `sudo kubevpn daemon` to the human, wait for them to run it, then retry once"

// wrapHint attaches an actionable hint to a daemon failure. Never wraps the
// sentinel cancellation.
func wrapHint(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w (hint: %s)", err, elevationHint)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./pkg/mcp/ -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add pkg/mcp/
git commit -m "feat(mcp): daemonAPI adapter, progress drain helper, elevation hint"
```

---

### Task 4: Read-only tools — `kubevpn_status`, `kubevpn_logs`, `kubevpn_version`

**Files:**
- Create: `pkg/mcp/tools_status.go`
- Modify: `pkg/mcp/tools.go` (register the three tools)
- Test: `pkg/mcp/tools_status_test.go`

**Interfaces:**
- Consumes: `daemonAPI`, `drainProgress`, `Options` (Tasks 2–3).
- Produces (registered tool names, used by docs/agents-guide.md): `kubevpn_status`, `kubevpn_logs`, `kubevpn_version`.

- [ ] **Step 1: Write the failing tests**

Create `pkg/mcp/tools_status_test.go`:

```go
package mcp

import (
	"context"
	"io"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

type fakeDaemon struct {
	rpc.DaemonClient // embed: unimplemented methods panic if called — pins the narrow surface

	version    *rpc.VersionResponse
	versionErr error
	status     *rpc.StatusResponse
	statusErr  error
	logsStream progressStream[rpc.LogResponse]
	logsErr    error
}

func (f *fakeDaemon) Version(context.Context, *rpc.VersionRequest) (*rpc.VersionResponse, error) {
	return f.version, f.versionErr
}

func (f *fakeDaemon) Status(context.Context, *rpc.StatusRequest) (*rpc.StatusResponse, error) {
	return f.status, f.statusErr
}

func (f *fakeDaemon) Logs(context.Context, *rpc.LogRequest) (rpc.Daemon_LogsClient, error) {
	if f.logsErr != nil {
		return nil, f.logsErr
	}
	// rpc.Daemon_LogsClient requires the full streaming interface; wrap via
	// grpc.ClientStream embedded no-op implementation.
	return &fakeLogsClient{progressStream: f.logsStream}, nil
}

type fakeLogsClient struct {
	grpc.ClientStream
	progressStream progressStream[rpc.LogResponse]
}

func (c *fakeLogsClient) RecvMsg(m *rpc.LogResponse) error { return c.progressStream.RecvMsg(m) }

func TestStatusToolPassthrough(t *testing.T) {
	d := &fakeDaemon{status: &rpc.StatusResponse{CurrentConnectionID: "c1"}}
	out, err := statusTool(context.Background(), d, StatusInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.Result, `"c1"`) {
		t.Fatalf("expected daemon JSON passthrough, got: %s", out.Result)
	}
}

func TestStatusToolSurfacesError(t *testing.T) {
	d := &fakeDaemon{statusErr: io.ErrClosedPipe}
	_, err := statusTool(context.Background(), d, StatusInput{})
	if err == nil || !strings.Contains(err.Error(), "hint") {
		t.Fatalf("expected error with hint, got %v", err)
	}
}

func TestLogsToolBoundedTail(t *testing.T) {
	d := &fakeDaemon{logsStream: &fakeStream{msgs: []string{"l1", "l2", "l3"}}}
	out, err := logsTool(context.Background(), d, LogsInput{Lines: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Lines=2 bounds the read: l1 must never be collected.
	if !strings.Contains(out.Result, "l2") || !strings.Contains(out.Result, "l3") || strings.Contains(out.Result, "l1") {
		t.Fatalf("expected tail [l2 l3], got: %s", out.Result)
	}
}

func TestVersionToolReportsClientAndDaemon(t *testing.T) {
	d := &fakeDaemon{version: &rpc.VersionResponse{Version: "v2.12.1"}}
	out, err := versionTool(context.Background(), d, Options{Version: "v1-test"}, VersionInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.Result, "v2.12.1") || !strings.Contains(out.Result, "v1-test") {
		t.Fatalf("expected both versions in output, got: %s", out.Result)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./pkg/mcp/ -run 'TestStatus|TestLogs|TestVersion' -v`
Expected: FAIL — `undefined: statusTool`, `undefined: StatusInput`, etc.

- [ ] **Step 3: Implement the tools**

Create `pkg/mcp/tools_status.go`:

```go
package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// StatusInput filters the status query.
type StatusInput struct {
	ConnectionID string `json:"connectionId,omitempty" jsonschema:"connection ID to query; empty lists all connections"`
}

// StatusOutput is the daemon's Status response serialized for the agent.
type StatusOutput struct {
	Result string `json:"result" jsonschema:"daemon StatusResponse as JSON"`
}

// statusTool is the plain function behind the kubevpn_status tool.
func statusTool(ctx context.Context, d daemonAPI, in StatusInput) (*StatusOutput, error) {
	resp, err := d.Status(ctx, &rpc.StatusRequest{ConnectionIDs: util.If(in.ConnectionID == "", nil, []string{in.ConnectionID})})
	if err != nil {
		return nil, wrapHint(err)
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("marshal status: %w", err)
	}
	return &StatusOutput{Result: string(b)}, nil
}

// LogsInput selects a bounded log tail.
type LogsInput struct {
	Lines        int32  `json:"lines,omitempty" jsonschema:"number of trailing lines to return (default 100)"`
	ConnectionID string `json:"connectionId,omitempty" jsonschema:"connection ID to read logs for; empty = current"`
}

// LogsOutput is the log tail serialized for the agent.
type LogsOutput struct {
	Result string `json:"result" jsonschema:"{lines: [...]} JSON"`
}

// logsTool reads at most in.Lines trailing log lines (non-follow). The bound
// is enforced by counting, not by a deadline: the fake and the real client
// both terminate on EOF, and Lines caps the collected output.
func logsTool(ctx context.Context, d daemonAPI, in LogsInput) (*LogsOutput, error) {
	if in.Lines <= 0 {
		in.Lines = 100
	}
	stream, err := d.Logs(ctx, &rpc.LogRequest{Follow: false, Lines: in.Lines, ConnectionID: in.ConnectionID})
	if err != nil {
		return nil, wrapHint(err)
	}
	var lines []string
	for len(lines) < int(in.Lines) {
		var resp rpc.LogResponse
		if err := stream.RecvMsg(&resp); err != nil {
			break // io.EOF or terminal error: return what we collected
		}
		lines = append(lines, resp.GetMessage())
	}
	b, err := json.Marshal(map[string]any{"lines": lines})
	if err != nil {
		return nil, fmt.Errorf("marshal logs: %w", err)
	}
	return &LogsOutput{Result: string(b)}, nil
}

// VersionInput takes no parameters.
type VersionInput struct{}

// VersionOutput reports both the MCP server (client) version and the daemon's.
type VersionOutput struct {
	Result string `json:"result" jsonschema:"{client, daemon} JSON"`
}

// versionTool reports client + daemon versions so an agent can detect skew.
func versionTool(ctx context.Context, d daemonAPI, opts Options, in VersionInput) (*VersionOutput, error) {
	resp, err := d.Version(ctx, &rpc.VersionRequest{})
	if err != nil {
		return nil, wrapHint(err)
	}
	b, err := json.Marshal(map[string]string{"client": opts.Version, "daemon": resp.GetVersion()})
	if err != nil {
		return nil, fmt.Errorf("marshal version: %w", err)
	}
	return &VersionOutput{Result: string(b)}, nil
}
```

Modify `pkg/mcp/server.go` — `Run` constructs the daemon client once and hands it to registration:

```go
// Run serves MCP over stdio until ctx is cancelled or stdin closes.
func Run(ctx context.Context, opts Options) error {
	cli, err := daemon.GetClient(false)
	if err != nil {
		return err
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "kubevpn", Version: opts.Version}, nil)
	registerTools(server, opts, cli)
	return server.Run(ctx, &mcp.StdioTransport{})
}
```

(`daemon` here is `pkg/daemon` — add the import `pkgdaemon "github.com/wencaiwulue/kubevpn/v2/pkg/daemon"` if the plain name collides with the `daemonAPI` identifier.)

Replace `pkg/mcp/tools.go`:

```go
package mcp

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerTools wires every MCP tool to its handler. Uninstall/Reset/Upgrade/Quit
// are deliberately absent (docs/56 §4): destructive operations stay human-only.
func registerTools(server *mcp.Server, opts Options, d daemonAPI) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "kubevpn_status",
		Description: "List KubeVPN connections and their state (namespace, cluster, TUN IP). Check before connecting: reuse an existing Connected session.",
		Annotations: mcp.ToolAnnotation{ReadOnlyHint: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in StatusInput) (*mcp.CallToolResult, StatusOutput, error) {
		return statusTool(ctx, d, in)
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "kubevpn_logs",
		Description: "Read the trailing N lines of KubeVPN daemon logs for a connection — the debugging channel when connect/proxy fails.",
		Annotations: mcp.ToolAnnotation{ReadOnlyHint: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in LogsInput) (*mcp.CallToolResult, LogsOutput, error) {
		return logsTool(ctx, d, in)
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "kubevpn_version",
		Description: "Report KubeVPN client and daemon versions.",
		Annotations: mcp.ToolAnnotation{ReadOnlyHint: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in VersionInput) (*mcp.CallToolResult, VersionOutput, error) {
		return versionTool(ctx, d, opts, in)
	})
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./pkg/mcp/ -v`
Expected: PASS (all prior tests too).

- [ ] **Step 5: Verify build**

Run: `go build ./...`
Expected: PASS. Adjust `mcp.AddTool`/`ToolAnnotation` mechanically if the resolved SDK version differs.

- [ ] **Step 6: Commit**

```bash
git add pkg/mcp/
git commit -m "feat(mcp): read-only tools — status, logs, version"
```

---

### Task 5: `kubevpn_connect` tool

**Files:**
- Create: `pkg/mcp/tools_connect.go`
- Modify: `pkg/mcp/tools.go` (register)
- Test: `pkg/mcp/tools_connect_test.go`

**Interfaces:**
- Consumes: `daemonAPI.Connect`, `drainProgress`, `Options` (KubeconfigBytes/Namespace/OriginKubeconfigPath).
- Produces: `kubevpn_connect` tool; `func connectTool(ctx, d daemonAPI, opts Options, in ConnectInput) (*ConnectOutput, error)`.

- [ ] **Step 1: Write the failing test**

Create `pkg/mcp/tools_connect_test.go`:

```go
package mcp

import (
	"context"
	"io"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// fakeBidiStream replays ConnectResponses then EOF; sends are recorded.
type fakeBidiStream struct {
	sent   []*rpc.ConnectRequest
	msgIDs []string
	err    error
}

func (f *fakeBidiStream) Send(req *rpc.ConnectRequest) error {
	f.sent = append(f.sent, req)
	return nil
}

func (f *fakeBidiStream) RecvMsg(m *rpc.ConnectResponse) error {
	if len(f.msgIDs) == 0 {
		if f.err != nil {
			return f.err
		}
		return io.EOF
	}
	m.ConnectionID = f.msgIDs[0]
	f.msgIDs = f.msgIDs[1:]
	return nil
}

type fakeConnectDaemon struct {
	fakeDaemon
	stream *fakeBidiStream
}

func (f *fakeConnectDaemon) Connect(context.Context) (grpcConnectClient, error) {
	return f.stream, nil
}
```

The generated client type is `grpc.BidiStreamingClient[rpc.ConnectRequest, rpc.ConnectResponse]` — alias it in `pkg/mcp/daemon.go` so tests and handlers share one name:

```go
// connectStream is the bidirectional Connect stream client.
type connectStream = grpc.BidiStreamingClient[rpc.ConnectRequest, rpc.ConnectResponse]
```

and `fakeConnectDaemon.Connect` returns `(connectStream, error)` — note `*fakeBidiStream` must satisfy that interface: it needs `Send(*rpc.ConnectRequest) error` (BidiStreamingClient has Send) plus the embedded `grpc.ClientStream` methods. Embed `grpc.ClientStream` in `fakeBidiStream` to fill the rest:

```go
type fakeBidiStream struct {
	grpc.ClientStream
	sent   []*rpc.ConnectRequest
	msgIDs []string
	err    error
}
```

Test body:

```go
func TestConnectToolReturnsConnectionIDOnEstablished(t *testing.T) {
	d := &fakeConnectDaemon{stream: &fakeBidiStream{msgIDs: []string{"conn-1"}}}
	opts := Options{KubeconfigBytes: []byte("kube"), Namespace: "default", OriginKubeconfigPath: "/tmp/kc"}
	out, err := connectTool(context.Background(), d, opts, ConnectInput{Namespace: "ns1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.ConnectionID != "conn-1" {
		t.Fatalf("want conn-1, got %q", out.ConnectionID)
	}
	sent := d.stream.sent[0]
	if sent.GetNamespace() != "ns1" || string(sent.GetKubeconfigBytes()) != "kube" {
		t.Fatalf("request fields not mapped: %+v", sent)
	}
}

func TestConnectToolDefaultsNamespaceFromOptions(t *testing.T) {
	d := &fakeConnectDaemon{stream: &fakeBidiStream{msgIDs: []string{"c"}}}
	_, err := connectTool(context.Background(), d, Options{KubeconfigBytes: []byte("k"), Namespace: "opt-ns"}, ConnectInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.stream.sent[0].GetNamespace() != "opt-ns" {
		t.Fatalf("want default namespace opt-ns, got %q", d.stream.sent[0].GetNamespace())
	}
}

func TestConnectToolWrapsFailureWithHint(t *testing.T) {
	d := &fakeConnectDaemon{stream: &fakeBidiStream{err: io.ErrClosedPipe}}
	_, err := connectTool(context.Background(), d, Options{KubeconfigBytes: []byte("k")}, ConnectInput{})
	if err == nil || !strings.Contains(err.Error(), "sudo kubevpn daemon") {
		t.Fatalf("expected elevation hint on failure, got %v", err)
	}
}
```

Also extend `fakeDaemon`'s embedded `rpc.DaemonClient`: nil embedded interface panics only if a method is invoked — Connect is overridden, fine.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./pkg/mcp/ -run TestConnect -v`
Expected: FAIL — `undefined: connectTool`, `undefined: ConnectInput`.

- [ ] **Step 3: Implement**

Create `pkg/mcp/tools_connect.go`:

```go
package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// ConnectInput starts a VPN session. Sessions live daemon-side; the stream
// closes (EOF) once established.
type ConnectInput struct {
	Namespace      string `json:"namespace,omitempty" jsonschema:"target namespace; empty = the default namespace of the resolved kubeconfig"`
	KubeconfigPath string `json:"kubeconfigPath,omitempty" jsonschema:"path to a kubeconfig file; empty = the path resolved at server start"`
}

// ConnectOutput reports the established session.
type ConnectOutput struct {
	ConnectionID string   `json:"connectionId" jsonschema:"ID of the established connection; pass to kubevpn_disconnect"`
	Namespace    string   `json:"namespace"`
	Messages     []string `json:"messages" jsonschema:"last progress messages from the daemon"`
}

// connectTool is the plain function behind the kubevpn_connect tool.
func connectTool(ctx context.Context, d daemonAPI, opts Options, in ConnectInput) (*ConnectOutput, error) {
	kubeconfigBytes := opts.KubeconfigBytes
	if in.KubeconfigPath != "" {
		b, err := os.ReadFile(in.KubeconfigPath)
		if err != nil {
			return nil, fmt.Errorf("read kubeconfig: %w", err)
		}
		kubeconfigBytes = b
	}
	ns := in.Namespace
	if ns == "" {
		ns = opts.Namespace
	}
	stream, err := d.Connect(ctx)
	if err != nil {
		return nil, wrapHint(err)
	}
	req := &rpc.ConnectRequest{
		KubeconfigBytes:      string(kubeconfigBytes),
		Namespace:            ns,
		OriginKubeconfigPath: opts.OriginKubeconfigPath,
		// Level 0 keeps daemon-side log verbosity at defaults; the agent reads
		// progress via the stream and kubevpn_logs.
		Level: 0,
	}
	if err := stream.Send(req); err != nil {
		return nil, wrapHint(fmt.Errorf("send connect request: %w", err))
	}
	// The daemon sends the connection ID in a response frame, so use the typed
	// drain (not the message-only drainProgress).
	connID, msgs, err := drainConnect(stream)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, wrapHint(err)
	}
	return &ConnectOutput{ConnectionID: connID, Namespace: ns, Messages: tail(msgs, 5)}, nil
}

// drainConnect consumes the Connect stream to its terminal state, returning
// the last non-empty ConnectionID and all progress messages; io.EOF on success.
func drainConnect(s connectStream) (string, []string, error) {
	var connID string
	var msgs []string
	for {
		var resp rpc.ConnectResponse
		if err := s.RecvMsg(&resp); err != nil {
			return connID, msgs, err
		}
		if resp.GetConnectionID() != "" {
			connID = resp.GetConnectionID()
		}
		if resp.GetMessage() != "" {
			msgs = append(msgs, resp.GetMessage())
		}
	}
}
```

Add `tail` to `pkg/mcp/daemon.go`:

```go
// tail returns the last n strings, original order.
func tail(ss []string, n int) []string {
	if len(ss) > n {
		return ss[len(ss)-n:]
	}
	return ss
}
```

Register in `pkg/mcp/tools.go` (inside `registerTools`):

```go
	mcp.AddTool(server, &mcp.Tool{
		Name:        "kubevpn_connect",
		Description: "Connect the local machine to a Kubernetes cluster network (VPN). Returns once established; the daemon owns the session. Scope proxies to this session afterwards.",
		Annotations: mcp.ToolAnnotation{ReadOnlyHint: false, DestructiveHint: ptr(false)},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ConnectInput) (*mcp.CallToolResult, ConnectOutput, error) {
		return connectTool(ctx, d, opts, in)
	})
```

(`ptr` = `k8s.io/utils/ptr`; if the SDK's `DestructiveHint` is not `*bool`, drop that field — `ReadOnlyHint: false` alone is correct.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./pkg/mcp/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/mcp/
git commit -m "feat(mcp): kubevpn_connect tool"
```

---

### Task 6: `kubevpn_proxy`, `kubevpn_leave`, `kubevpn_disconnect` tools

**Files:**
- Create: `pkg/mcp/tools_proxy.go`
- Modify: `pkg/mcp/tools.go` (register the three)
- Test: `pkg/mcp/tools_proxy_test.go`

**Interfaces:**
- Consumes: `daemonAPI.Proxy/Leave/Disconnect`, `drainProgress`, `tail`.
- Produces: `kubevpn_proxy`, `kubevpn_leave`, `kubevpn_disconnect` tools; plain functions `proxyTool`, `leaveTool`, `disconnectTool` (same `(ctx, d, opts, in)` shape as `connectTool`).

- [ ] **Step 1: Write the failing tests**

Create `pkg/mcp/tools_proxy_test.go`:

```go
package mcp

import (
	"context"
	"io"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// proxyStream replays ProxyResponses then EOF; records the sent request.
type proxyStream struct {
	grpcSendErr error
	sent        []*rpc.ProxyRequest
	msgs        []string
	err         error
}

func (f *proxyStream) Send(req *rpc.ProxyRequest) error {
	if f.grpcSendErr != nil {
		return f.grpcSendErr
	}
	f.sent = append(f.sent, req)
	return nil
}

func (f *proxyStream) RecvMsg(m *rpc.ProxyResponse) error {
	if len(f.msgs) == 0 {
		if f.err != nil {
			return f.err
		}
		return io.EOF
	}
	m.Message = f.msgs[0]
	f.msgs = f.msgs[1:]
	return nil
}

type fakeProxyDaemon struct {
	fakeDaemon
	stream *proxyStream
}

func (f *fakeProxyDaemon) Proxy(context.Context) (proxyClient, error) { return f.stream, nil }

func TestProxyToolRequiresWorkloads(t *testing.T) {
	_, err := proxyTool(context.Background(), &fakeProxyDaemon{stream: &proxyStream{}}, Options{}, ProxyInput{})
	if err == nil || !strings.Contains(err.Error(), "workload") {
		t.Fatalf("expected workload validation error, got %v", err)
	}
}

func TestProxyToolRejectsBareProxyWithoutHeaders(t *testing.T) {
	d := &fakeProxyDaemon{stream: &proxyStream{}}
	_, err := proxyTool(context.Background(), d, Options{}, ProxyInput{Workloads: []string{"deploy/web"}, Namespace: "ns"})
	if err == nil || !strings.Contains(err.Error(), "headers") {
		t.Fatalf("expected header-scoping guard, got %v", err)
	}
	if len(d.stream.sent) != 0 {
		t.Fatalf("guard must reject before sending any RPC")
	}
}

func TestProxyToolMapsRequestFields(t *testing.T) {
	d := &fakeProxyDaemon{stream: &proxyStream{msgs: []string{"injected"}}}
	out, err := proxyTool(context.Background(), d,
		Options{KubeconfigBytes: []byte("kc")},
		ProxyInput{Workloads: []string{"deploy/web"}, Namespace: "ns", Headers: map[string]string{"X-KubeVPN-User": "agent"}},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sent := d.stream.sent[0]
	if sent.GetNamespace() != "ns" || len(sent.GetWorkloads()) != 1 || sent.GetHeaders()["X-KubeVPN-User"] != "agent" {
		t.Fatalf("request fields not mapped: %+v", sent)
	}
	if len(out.Messages) != 1 || out.Messages[0] != "injected" {
		t.Fatalf("messages not surfaced: %+v", out.Messages)
	}
}
```

Type alias needed in `pkg/mcp/daemon.go`:

```go
// proxyStreamClient is the bidirectional Proxy stream client.
type proxyStreamClient = grpc.BidiStreamingClient[rpc.ProxyRequest, rpc.ProxyResponse]
```

(Also add `leaveStreamClient` and `disconnectStreamClient` aliases the same way for `rpc.LeaveRequest/LeaveResponse` and `rpc.DisconnectRequest/DisconnectResponse`; the fakes embed `grpc.ClientStream` exactly like `fakeBidiStream`.)

Plus leave/disconnect test cases in the same file:

```go
func TestLeaveToolMapsRequestFields(t *testing.T) {
	// fakeLeaveDaemon mirrors fakeProxyDaemon with rpc.LeaveRequest/response.
	d := newFakeLeaveDaemon([]string{"restored"})
	out, err := leaveTool(context.Background(), d, Options{}, LeaveInput{Namespace: "ns", Workloads: []string{"deploy/web"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.lastLeave.GetNamespace() != "ns" || len(d.lastLeave.GetWorkloads()) != 1 {
		t.Fatalf("request fields not mapped: %+v", d.lastLeave)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("messages not surfaced: %+v", out.Messages)
	}
}

func TestDisconnectToolDefaultsToCurrent(t *testing.T) {
	d := newFakeDisconnectDaemon()
	_, err := disconnectTool(context.Background(), d, Options{}, DisconnectInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.lastDisconnect.GetConnectionID() != nil && *d.lastDisconnect.GetConnectionID() != "" {
		t.Fatalf("empty input must leave ConnectionID unset (daemon defaults to current)")
	}
}

func TestDisconnectToolByID(t *testing.T) {
	d := newFakeDisconnectDaemon()
	id := "conn-9"
	_, err := disconnectTool(context.Background(), d, Options{}, DisconnectInput{ConnectionID: id})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.lastDisconnect.GetConnectionID() == nil || *d.lastDisconnect.GetConnectionID() != id {
		t.Fatalf("want ConnectionID=%q, got %+v", id, d.lastDisconnect)
	}
}
```

The three `newFake*` helpers are small concrete structs defined in the same test file, mirroring `fakeProxyDaemon`: `fakeLeaveDaemon` implements `Leave(ctx) (leaveStreamClient, error)` recording `lastLeave *rpc.LeaveRequest` and replaying `LeaveResponse.Message` list; `fakeDisconnectDaemon` implements `Disconnect(ctx) (disconnectStreamClient, error)` recording `lastDisconnect *rpc.DisconnectRequest`, replaying EOF. Write them as full code in the test file (same pattern as `fakeProxyDaemon` above).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./pkg/mcp/ -run 'TestProxy|TestLeave|TestDisconnect' -v`
Expected: FAIL — `undefined: proxyTool`, `undefined: ProxyInput`, etc.

- [ ] **Step 3: Implement**

Create `pkg/mcp/tools_proxy.go`:

```go
package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// ProxyInput intercepts workloads. Header scoping is mandatory from an agent:
// a bare proxy routes ALL traffic on declared ports to this machine.
type ProxyInput struct {
	Namespace string            `json:"namespace,omitempty" jsonschema:"workload namespace; empty = server default"`
	Workloads []string          `json:"workloads" jsonschema:"workloads to proxy, e.g. deploy/web or svc/orders"`
	Headers   map[string]string `json:"headers,omitempty" jsonschema:"REQUIRED from agents — traffic filter headers, e.g. {\"X-KubeVPN-User\":\"agent1\"}"`
	PortMap   []string          `json:"portMap,omitempty" jsonschema:"optional port mappings, e.g. 8888:8080"`
}

// ProxyOutput reports the result of the proxy operation.
type ProxyOutput struct {
	Messages []string `json:"messages" jsonschema:"progress messages from the daemon"`
}

// proxyTool is the plain function behind the kubevpn_proxy tool.
func proxyTool(ctx context.Context, d daemonAPI, opts Options, in ProxyInput) (*ProxyOutput, error) {
	if len(in.Workloads) == 0 {
		return nil, fmt.Errorf("at least one workload is required")
	}
	if len(in.Headers) == 0 {
		return nil, fmt.Errorf("agents must scope proxy with headers (docs/agents-guide.md): pass headers such as {\"X-KubeVPN-User\": \"<agent-id>\"} to avoid routing other users' traffic to this machine")
	}
	stream, err := d.Proxy(ctx)
	if err != nil {
		return nil, wrapHint(err)
	}
	req := &rpc.ProxyRequest{
		KubeconfigBytes:      string(opts.KubeconfigBytes),
		Namespace:            in.Namespace,
		Headers:              in.Headers,
		PortMap:              in.PortMap,
		Workloads:            in.Workloads,
		OriginKubeconfigPath: opts.OriginKubeconfigPath,
		Level:                0,
	}
	if err := stream.Send(req); err != nil {
		return nil, wrapHint(fmt.Errorf("send proxy request: %w", err))
	}
	_, msgs, err := drainProgress(stream)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, wrapHint(err)
	}
	return &ProxyOutput{Messages: tail(msgs, 5)}, nil
}

// LeaveInput restores workloads to their original spec (undo proxy).
type LeaveInput struct {
	Namespace string   `json:"namespace,omitempty"`
	Workloads []string `json:"workloads,omitempty" jsonschema:"workloads to leave; empty = all resources owned by this session"`
}

// LeaveOutput reports the leave result.
type LeaveOutput struct {
	Messages []string `json:"messages"`
}

// leaveTool is the plain function behind the kubevpn_leave tool.
func leaveTool(ctx context.Context, d daemonAPI, opts Options, in LeaveInput) (*LeaveOutput, error) {
	stream, err := d.Leave(ctx)
	if err != nil {
		return nil, wrapHint(err)
	}
	req := &rpc.LeaveRequest{Namespace: in.Namespace, Workloads: in.Workloads, Level: 0}
	if err := stream.Send(req); err != nil {
		return nil, wrapHint(fmt.Errorf("send leave request: %w", err))
	}
	_, msgs, err := drainProgress(stream)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, wrapHint(err)
	}
	return &LeaveOutput{Messages: tail(msgs, 5)}, nil
}

// DisconnectInput ends a VPN session. Empty input disconnects the current session.
type DisconnectInput struct {
	ConnectionID string `json:"connectionId,omitempty" jsonschema:"connection to disconnect; empty = current"`
}

// DisconnectOutput reports the disconnect result.
type DisconnectOutput struct {
	Messages []string `json:"messages"`
}

// disconnectTool is the plain function behind the kubevpn_disconnect tool.
func disconnectTool(ctx context.Context, d daemonAPI, opts Options, in DisconnectInput) (*DisconnectOutput, error) {
	stream, err := d.Disconnect(ctx)
	if err != nil {
		return nil, wrapHint(err)
	}
	req := &rpc.DisconnectRequest{Level: 0}
	if in.ConnectionID != "" {
		req.ConnectionID = &in.ConnectionID
	}
	if err := stream.Send(req); err != nil {
		return nil, wrapHint(fmt.Errorf("send disconnect request: %w", err))
	}
	_, msgs, err := drainProgress(stream)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, wrapHint(err)
	}
	return &DisconnectOutput{Messages: tail(msgs, 5)}, nil
}
```

Note the generated `DisconnectRequest.ConnectionID` is `optional string` → Go field is `*string` with getter `GetConnectionID() string`; the `&in.ConnectionID` assignment above matches.

Register in `pkg/mcp/tools.go`:

```go
	mcp.AddTool(server, &mcp.Tool{
		Name:        "kubevpn_proxy",
		Description: "Proxy a cluster workload's traffic to this machine with header-based scoping. ALWAYS pass headers; leave the workload when finished.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ProxyInput) (*mcp.CallToolResult, ProxyOutput, error) {
		return proxyTool(ctx, d, opts, in)
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "kubevpn_leave",
		Description: "Restore proxied workloads to their original spec. Always call before disconnecting.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in LeaveInput) (*mcp.CallToolResult, LeaveOutput, error) {
		return leaveTool(ctx, d, opts, in)
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "kubevpn_disconnect",
		Description: "End a KubeVPN VPN session. Empty connectionId disconnects the current session.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in DisconnectInput) (*mcp.CallToolResult, DisconnectOutput, error) {
		return disconnectTool(ctx, d, opts, in)
	})
```

Also add the two remaining type aliases in `pkg/mcp/daemon.go`:

```go
// leaveStreamClient is the bidirectional Leave stream client.
type leaveStreamClient = grpc.BidiStreamingClient[rpc.LeaveRequest, rpc.LeaveResponse]

// disconnectStreamClient is the bidirectional Disconnect stream client.
type disconnectStreamClient = grpc.BidiStreamingClient[rpc.DisconnectRequest, rpc.DisconnectResponse]
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./pkg/mcp/ -v`
Expected: PASS (all).

- [ ] **Step 5: Verify full loop**

Run: `go build ./... && go vet ./pkg/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add pkg/mcp/
git commit -m "feat(mcp): proxy, leave, disconnect tools"
```

---

### Task 7: `kubevpn mcp` subcommand

**Files:**
- Create: `cmd/kubevpn/cmds/mcp.go`
- Modify: `cmd/kubevpn/cmds/root.go` (one registration line in the "Management commands" group)
- Test: none (thin wiring; the behavior under test lives in pkg/mcp). Compile + smoke only.

**Interfaces:**
- Consumes: `mcp.Run(ctx, Options)`, `util.ConvertToKubeConfigBytes(f)`, `util.GetKubeConfigPath(f)`, `config.Version`.
- Produces: the `kubevpn mcp` command.

- [ ] **Step 1: Write the command**

Create `cmd/kubevpn/cmds/mcp.go`:

```go
package cmds

import (
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	pkgmcp "github.com/wencaiwulue/kubevpn/v2/pkg/mcp"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func CmdMCP(f cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Start the MCP (Model Context Protocol) stdio server for AI coding agents",
		Long: `Start a Model Context Protocol server over stdio exposing KubeVPN's session
workflow (status, connect, proxy, leave, disconnect, logs) to AI coding agents.
Destructive operations (uninstall, reset, quit) are intentionally not exposed.
See https://kubevpn.dev for the agent usage guide.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			bytes, ns, err := util.ConvertToKubeConfigBytes(f)
			if err != nil {
				return err
			}
			opts := pkgmcp.Options{
				KubeconfigBytes:      bytes,
				Namespace:            ns,
				OriginKubeconfigPath: util.GetKubeConfigPath(f),
				Version:              config.Version,
			}
			return pkgmcp.Run(cmd.Context(), opts)
		},
	}
	return cmd
}
```

Adjust the import alias/style to match neighboring files (`cmds/connect.go` uses the same `util` helpers — copy its import block style). Note: `genericclioptions` is imported only if needed — drop it if unused after matching the file style.

- [ ] **Step 2: Register in the root command**

In `cmd/kubevpn/cmds/root.go`, inside the `"Management commands:"` group, add one line after `CmdStatus(factory),`:

```go
				CmdMCP(factory),
```

- [ ] **Step 3: Verify build and help output**

Run: `go build ./... && go run ./cmd/kubevpn mcp --help`
Expected: build PASS; help text shows the `mcp` subcommand with its Short description.

- [ ] **Step 4: Verify the frozen-CLI constraint**

Run: `git diff --stat -- cmd/`
Expected: exactly two files — `cmd/kubevpn/cmds/mcp.go` (new) and `cmd/kubevpn/cmds/root.go` (one added line). If anything else under `cmd/` changed, revert it.

- [ ] **Step 5: Commit**

```bash
git add cmd/kubevpn/cmds/mcp.go cmd/kubevpn/cmds/root.go
git commit -m "feat: add `kubevpn mcp` subcommand (MCP stdio server for AI agents)"
```

---

### Task 8: Docs sync + full verification + manual smoke

**Files:**
- Modify: `README.md` and `README_ZH.md` (bilingual 1:1 — add `kubevpn mcp` to the command list wherever `kubevpn status` is listed)
- Modify: `docs/56-ai-agent-access.md` (flip the status line from `**planned**` to `**implemented** (P1 in docs/agents-guide.md, P2 as `kubevpn mcp`)`)
- Modify: `docs/README.md` (move 56 out of "Planned Directions" into the docs index body if present there)

**Interfaces:**
- Consumes: everything shipped in Tasks 1–7.
- Produces: the merged, verified state.

- [ ] **Step 1: Update the three doc files**

README.md / README_ZH.md: locate the section listing example commands (§ "Example Commands"), add one bilingual-matching line each:

```markdown
# Start the MCP server for AI coding agents (stdio)
kubevpn mcp
```

README_ZH.md equivalent:

```markdown
# 为 AI 编码代理启动 MCP 服务器（stdio）
kubevpn mcp
```

docs/56: replace `> Status: **planned** — ...` with:

```markdown
> Status: **implemented** — P1 lives in [agents-guide.md](agents-guide.md); P2 is the
> `kubevpn mcp` subcommand (`cmd/kubevpn/cmds/mcp.go` + `pkg/mcp`).
```

docs/README.md: move the 56 entry out of "Planned Directions" up next to "Upgrade & Operations" (rename its section header to "AI Agent Access" or place under an existing fitting section).

- [ ] **Step 2: Run the full verification loop**

Run:
```bash
go build ./...
go vet ./pkg/...
go test ./pkg/...
make ut-compile
```
Expected: all PASS.

- [ ] **Step 3: Manual MCP smoke test**

Run:
```bash
go run ./cmd/kubevpn mcp < /dev/null
```
Expected: the server starts, reads EOF on stdin, and exits cleanly (no panic).

Then, if Claude Code is available:
```bash
claude mcp add kubevpn -- go run /Users/fengcaiwen/GolandProjects/kubevpn/cmd/kubevpn mcp
```
and in a fresh Claude Code session ask the agent to run the `kubevpn_version` tool. Expected: JSON containing both versions.

- [ ] **Step 4: Commit**

```bash
git add README.md README_ZH.md docs/56-ai-agent-access.md docs/README.md
git commit -m "docs: sync AI agent access docs after implementing kubevpn mcp"
```

---

## Self-Review Notes (already applied while writing)

- Spec coverage: docs/56 §3 (guide) → Task 1; §4 entry point → Task 7; tool table → Tasks 4–6; exclusions (Uninstall/Reset/Upgrade/Quit) → enforced by omission in `registerTools` (Task 4/6); elevation handoff → `wrapHint` + guide §"Elevation handoff" (Tasks 1, 3); testing contract → fakes per task, no-cluster only.
- Type consistency: output structs carry `Result string \`json:"result"\`` (Task 4 fix applied inline); `connectStream`/`proxyStreamClient`/`leaveStreamClient`/`disconnectStreamClient` aliases defined once in `pkg/mcp/daemon.go` and used by fakes; `drainProgress` error contract is `io.EOF` on success (test + implementation agree).
- Known mechanical adjustments allowed at execution: MCP SDK `AddTool`/`ToolAnnotation` signature drift (Task 2 Step 4 note) — same semantics, consult `go doc`.
