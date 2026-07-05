# Command Progress (kind-style spinner)

## 1. Overview

Every operational `kubevpn` command (`connect`, `proxy`, `sync`, `leave`, `reset`, `unsync`,
`disconnect`, `uninstall`, `quit`) renders its steps as a
[kind](https://github.com/kubernetes-sigs/kind)-style checklist: each step animates as a spinner while
it runs and finalizes with a green `✓` (or `✗` on failure), carrying the data discovered during the
step. (`logs` is the one exception — it tails the daemon log files verbatim and never animates.) A
successful connect looks like:

```
Connecting to the cluster ...
 ✓ Using manager namespace "kubevpn"
 ✓ Using existing traffic manager in namespace "kubevpn"
 ✓ Detected cluster CIDRs: 10.0.0.0/8, 172.16.0.0/12
 ✓ Detected service CIDR: 10.96.0.0/12
 ✓ Forwarded ports (TCP/UDP/xDS)
 ✓ Created TUN device "utun5"
 ✓ Allocated TUN IP 198.18.0.5 / 2001:2::5
 ✓ Added 3 Pod and Service routes
 ✓ Configured DNS (cluster DNS 10.96.0.10)
 ✓ Wrote 12 services in namespace "default" to the hosts file
Connected. You can now access the Kubernetes cluster.
```

On a TTY the first line (`Connecting to the cluster ...`) is rendered in **bold** and the final line
(`Connected. ...`) in **bold green**, framing the green `✓` steps between them; on non-TTY output both
print as plain text. The heading is a step *kind* (§3); the success terminus is printed by the CLI via
`progress.Success` (§4), not streamed.

## 2. The challenge: progress is produced in the daemon, animated in the CLI

Connect steps run in the daemons (the user daemon resolves the manager namespace and traffic manager;
the root daemon does CIDR detection, port-forward, TUN, routes, DNS — see
[02-dual-daemon.md](02-dual-daemon.md)). Their log messages are streamed to the CLI as plain text over
gRPC (`ConnectResponse.message`) and printed append-only — see [13-logging-architecture.md](13-logging-architecture.md).

A spinner, however, must be **rendered on the CLI side**, because only the CLI owns the terminal. So
the daemon cannot draw the spinner; it can only *mark* which messages are step boundaries, and the CLI
turns those marks into animation. The user daemon forwards the root daemon's stream verbatim
(`CopyGRPCConnStream` / `CopyAndConvertGRPCStream`), so a single CLI render point sees every step from
both daemons.

This is also why `proxy` and `sync` get the connect steps for free: their daemon actions internally
call `Connect` and forward its stream, so the CIDR/TUN/route/DNS steps flow straight through to the
same CLI renderer before the command's own steps (sidecar injection, file sync) are appended.

## 3. Step protocol (marker travels on the stream, not in the log file)

Steps are emitted with three helpers in `pkg/log`:

| Helper | Meaning | CLI effect |
|---|---|---|
| `plog.StepTitle(ctx, msg)` | a bold heading preceding a group of steps | print the line in **bold** (no spinner, no `✓`) |
| `plog.StepStart(ctx, msg)` | a step began (present-continuous text) | start/refresh the spinner line |
| `plog.StepDone(ctx, format, args...)` | the step succeeded (text with data) | finalize the line with `✓` |

The helpers attach a logrus field (`_kubevpn_step = title|start|done`) rather than mutating the message text.
Two outputs then diverge from the same log entry (the "one logger, two outputs" rule of
[13-logging-architecture.md](13-logging-architecture.md)):

- **gRPC stream → CLI**: `StreamHook.Fire` sees the field and prepends a sentinel to the streamed
  message — `\x1fS` (begin), `\x1fD` (done), or `\x1fT` (heading/title). `\x1f` (ASCII Unit Separator)
  is valid UTF-8 and never appears in normal messages. Encoding/decoding is centralized in
  `EncodeStep`/`DecodeStep`.
- **daemon log file**: `serverFormat` strips the internal field, so the file stays clean
  (`... info: Forwarding ports`) with no sentinel and no field noise.

The wire format is owned by one symmetric pair in `pkg/log/logger.go`: `EncodeStep(kind, msg)`
produces the sentinel-prefixed string and `DecodeStep(msg)` recovers `(StepKind, text)`, where
`StepKind` is `StepNone | StepBegin | StepEnd | StepHeading` (the `StepTitle` helper emits
`StepHeading` — the helper/kind names parallel the existing `StepStart`/`StepBegin` pair). Nothing
else hard-codes the `\x1f` bytes — both the
`StreamHook` (producer) and the CLI `Renderer` (consumer) go through this pair, so the encoding can
change in exactly one place (`TestEncodeDecodeStep_RoundTrip` pins the round-trip).

> **`StepDone` may be called without a preceding `StepStart`.** A step that resolves instantly (it has
> no meaningful "in progress" phase) emits only `StepDone` — e.g. the manager-namespace resolution
> (`connect_elevate.go`), reuse of an existing traffic manager (`traffmgr.go`), and the cached-CIDR
> path (`connect.go`). The CLI renders these as a standalone `✓` line with no spinner animation
> (see §4).

### Sentinel stripping for non-spinner consumers

The sentinel is meaningful only to the CLI spinner. Every *other* consumer of a step-bearing stream
strips it via `plog.DecodeStep`, so the raw `\x1f` never leaks:

- `grpcutil.PrintGRPCStream` (the generic writer used by the `logs` command, the `run` mode, and the
  daemon's reconnect-from-persistence path that writes to the log file) decodes each message and writes
  only the text.
- `daemon/action/sync.go`'s `CopyAndConvertGRPCStream` callback forwards the message to the CLI
  **with** the sentinel (for spinner rendering) but writes the **decoded** text to the log file.

## 4. CLI renderer (`pkg/util/progress`)

`progress.Renderer` consumes the stream and drives the animation. It is a thin adapter over
**`github.com/theckman/yacspin`**, which owns the animation goroutine, TTY detection, Windows VT
handling (via `fatih/color` + `go-colorable`), and the non-TTY fallback.

All commands share a single generic CLI entry point, `printProgressStream[T]`
(`cmd/kubevpn/cmds/progress.go`), so there is exactly one render loop to reason about:

```go
func printProgressStream[T any](ctx, stream, out) (connID string, err error) {
    r := progress.New(out)            // nil out → drain without rendering
    defer r.Stop()
    for {
        t := new(T)
        stream.RecvMsg(t)             // EOF → return
        // capture connID via the connectionIDer interface (ConnectResponse only)
        r.Write(t.(grpcutil.Printable).GetMessage())   // one line per message
    }
}
```

`printConnectGRPCStream` is now a one-line wrapper over `printProgressStream[rpc.ConnectResponse]`;
`proxy`/`sync`/`leave`/`reset`/`unsync`/`disconnect`/`uninstall`/`quit` call it with their own response
type. The returned `connID` is used by `connect`/`disconnect` for managed-proxy bookkeeping; commands
that don't carry one simply get `""` (the `connectionIDer` type assertion never matches).

`Write` decodes each message with `plog.DecodeStep` and maps it to yacspin:

- **heading** → print the text in **bold** (no spinner, no `✓`); if a step is mid-flight, `Pause()` /
  print / `Unpause()` so the heading lands on its own line
- **begin** → `spinner.Message(text)` + `Start()` (animate this step)
- **done, spinner running** → `spinner.StopMessage(text)` + `Stop()` (print ` ✓ text`, return to stopped)
- **done, no spinner running** → print ` ✓ text` directly. This handles the `StepDone`-without-`StepStart`
  case (§3): instant steps never started a spinner, so there is nothing to finalize — the line is
  emitted standalone. (Same branch covers the non-TTY fallback where `spinner == nil`.)
- **plain log line** → `Pause()` / print above / `Unpause()` so logs scroll above the live spinner
- `Stop()` while a step is still running → `StopFail()` (`✗`), so an interrupted step is never
  falsely reported as done

yacspin supports `Start`→`Stop`→`Start` on one instance, which is what makes the one-`✓`-per-step
checklist work. The renderer is **not** hand-rolled — kind itself hand-rolls a small spinner, but
yacspin is purpose-built for "a serial list of tasks each ending in ✓/✗", is concurrency-safe, and all
of its dependencies were already vendored, so it adds no new transitive dependencies.

### Non-TTY fallback

When stdout is not a terminal (pipes, CI, log capture, the `kubevpn run` container-mode success scan in
`pkg/util/docker.go`), yacspin auto-detects it and degrades to non-animated, line-by-line output.

The bold heading and the bold-green success line use the same fallback rule. Both write raw ANSI
(yacspin's check marks already do — its writer is `os.Stdout`, not a colorable wrapper, so this renders
identically wherever the `✓` shows color) gated on a `golang.org/x/term` TTY check; on a non-terminal
writer they print plain. The final success line (`config.Slogan`) is printed by the CLI command after
the stream ends via `progress.Success` (centralized in `printSlogan`, `cmd/kubevpn/cmds/progress.go`),
so the plain text still matches and the sentinel-based success detection in container mode keeps
working.

## 5. Wording style

Step text is normalized so the checklist reads uniformly across commands:

- **`StepStart`** — present-continuous, `"<Verb-ing> <object>"`, terse, no trailing punctuation:
  `Forwarding ports`, `Injecting proxy sidecar`, `Removing proxy from workloads`.
- **`StepDone`** — past tense plus the data discovered, `"<Verb-ed> <object> [(detail)]"`:
  `Forwarded ports (TCP/UDP/xDS)`, `Injected proxy sidecar into 2 workloads`.
- Identifiers (namespace, workload, device, IP) use `%q`; counts use `%d`.
- Abbreviations are fixed: `TCP/UDP`, `xDS`, `DNS`, `CIDR`, `TUN`, `IP`, `IPv4`/`IPv6`.
- Per-item detail (one line per workload / DNS sub-op / CIDR probe) is logged at `Debug`, so a step is a
  single `Start`→`Done` pair on screen and the noise only appears with `--debug`.

## 6. Step inventory

### connect (also flows through `proxy` and `sync`)

| Step | Done-only? | Where it is emitted |
|---|---|---|
| heading `Connecting to the cluster ...` (bold, `StepTitle`) | — | `pkg/daemon/action/connect_elevate.go` |
| `Using namespace %q` / `Using manager namespace %q` | ✅ | `connect_elevate.go` (`detectAndSetManagerNamespace`) |
| `Using traffic manager in namespace %q` (done-only) / `Creating traffic manager` → `Created traffic manager in namespace %q` | partial | `pkg/handler/traffmgr.go` |
| `Detecting cluster CIDRs` → `Detected cluster CIDRs: …` / `Detecting service CIDR` → `Detected service CIDR: …` | — | `pkg/util/cidr.go` |
| `Detected cluster CIDRs: … (cached)` | ✅ | `pkg/handler/connect.go` (cached path) |
| `Forwarding ports` → `Forwarded ports (TCP/UDP/xDS)`, `Creating TUN device` → `Created TUN device %q`, `Allocating TUN IP` → `Allocated TUN IP %s`, `Adding routes` → `Added %d pod/service routes`, `Configuring DNS` → `Configured DNS (cluster DNS %s)`, `Writing service records to the hosts file` → `Wrote %d service records to the hosts file (namespace %q)` | — | `pkg/handler/network.go` |

### per-command steps

| Command | StepStart → StepDone | Where |
|---|---|---|
| `proxy` (after connect) | `Injecting proxy sidecar` → `Injected proxy sidecar into %d workloads` | `pkg/handler/connect.go` (`CreateRemoteInboundPod`) |
| `leave` | `Removing proxy from workloads` → `Removed proxy from %d workloads` | `pkg/handler/leave.go` (`LeaveResource`) |
| `reset` | `Resetting workloads` → `Reset %d workloads` | `pkg/handler/reset.go` (`Reset`) |
| `sync` (after connect) | `Syncing files` → `Synced files for %d workloads` | `pkg/handler/sync.go` (`DoSync`) |
| `unsync` | `Stopping file sync` → `Stopped file sync for %d workloads` | `pkg/handler/sync.go` (`Cleanup`) |
| `uninstall` | `Uninstalling traffic manager` → `Uninstalled traffic manager from namespace %q` | `pkg/daemon/action/uninstall.go` |
| `disconnect` | `Disconnecting` → `Disconnected from the cluster` | `pkg/daemon/action/disconnect.go` (user daemon only) |
| `quit` | `Cleaning up connections` → `Cleaned up %d connections` | `pkg/daemon/action/quit.go` (only when connections exist) |

"Done-only" marks steps that emit only `StepDone` (no spinner; rendered as a standalone `✓` — see §3).
The terminal success line `config.Slogan` (`Connected. …`) is **not** a step: the CLI prints it after
the stream ends via `progress.Success` (bold green on a TTY, plain otherwise — `printSlogan` in
`cmd/kubevpn/cmds/progress.go`), so non-TTY success detection keeps working (see §4 "Non-TTY fallback").

> **Double-render guard.** `disconnect` runs in both daemons (the user daemon forwards to the sudo
> daemon and copies its stream). The step is emitted only from the user daemon (`!svr.IsSudo`) so the
> CLI never sees it twice. `quit` is immune — the CLI calls each daemon's `Quit` on a separate
> sequential stream — so it emits unconditionally, but skips the step when a daemon holds no
> connections.

## 7. Tests

The feature has no cluster dependency, so it is covered by plain (non-`integration`) tests at the
seams of the protocol:

| Test | File | What it pins |
|---|---|---|
| `TestStep_StreamCarriesSentinel_FileStaysClean` | `pkg/log/step_test.go` | the two-output contract: one `StepStart`+`StepDone` pair yields sentinel-encoded **stream** lines (decode back to `StepBegin`/`StepEnd` + clean text) while the **log file** has no sentinel bytes and no `_kubevpn_step` field |
| `TestEncodeDecodeStep_RoundTrip` | `pkg/log/step_test.go` | `DecodeStep(EncodeStep(k, msg))` round-trips for every `StepKind` — the wire format stays symmetric |
| `TestRenderer_NonTTY` | `pkg/util/progress/spinner_test.go` | a `*bytes.Buffer` (not a TTY) → yacspin degrades to line-by-line output; ordinary log lines pass through and a finished step renders `✓` + its done text |
| `TestRenderer_NonTTY_PlainOnly` | `pkg/util/progress/spinner_test.go` | a stream with no step markers (e.g. the header line) passes through verbatim |
| `TestPrintGRPCStream_StripsStepSentinel` | `pkg/daemon/grpcutil/stream_test.go` | the non-spinner writer decodes the sentinel away, so log file / `run` / `logs` never emit raw `\x1f` |
| `TestPrintProgressStream_RendersStepsAndCapturesConnID` | `cmd/kubevpn/cmds/progress_test.go` | the shared CLI renderer drives a fake stream end to end: sentinel stripped, finished step shows `✓` + done text, and `ConnectionID` is captured |

Renderer tests assert on **substrings**, not exact framing: yacspin owns the spinner glyphs, spacing,
and ANSI control bytes, so pinning the full output would couple the test to the vendored library's
internals.

```bash
go test ./pkg/log/... ./pkg/util/progress/... ./pkg/daemon/grpcutil/... ./cmd/...
```

## 8. Related Files

| File | Purpose |
|---|---|
| `pkg/log/context.go` | `StepStart` / `StepDone` helpers |
| `pkg/log/logger.go` | `StreamHook` sentinel injection, `EncodeStep` / `DecodeStep`, `serverFormat` field stripping |
| `pkg/util/progress/spinner.go` | `Renderer` (yacspin adapter) |
| `cmd/kubevpn/cmds/progress.go` | `printProgressStream[T]` — the shared CLI renderer entry point for all commands |
| `pkg/daemon/grpcutil/stream.go` | `PrintGRPCStream` (non-spinner writer; strips the step sentinel) |
| `pkg/log/step_test.go` | two-output contract + encode/decode round-trip |
| `pkg/util/progress/spinner_test.go` | non-TTY renderer behavior |
| `cmd/kubevpn/cmds/connect.go` | `printConnectGRPCStream` (one-line wrapper over `printProgressStream`) |
| `docs/13-logging-architecture.md` | log routing the protocol builds on |
| `docs/02-dual-daemon.md` | which daemon emits which step |
