# eBPF Feasibility for KubeVPN

## 1. The Question

Can eBPF be used in this project — for throughput, to replace iptables, for kernel-level
observability, or at all? This document answers each separately, because they have different
answers, and records why, so the question does not have to be re-litigated from scratch.

Short version: **three of the four are no, and the fourth is "worth doing only under conditions
that do not hold today"**. There is currently no eBPF anywhere in the tree.

## 2. What This Project's Shape Forces

Any eBPF work inherits these, none of which are negotiable in the near term.

| Constraint | Where | Consequence |
|---|---|---|
| `CGO_ENABLED=0` for all 8 release targets | `Makefile` | cilium/ebpf is pure Go, so loading is fine. **Compiling** the BPF objects needs clang/LLVM + `bpf2go` — a non-Go toolchain in a project that today needs only `go build` |
| Targets include darwin×2, windows×3, and **linux/386** | `Makefile` | eBPF is Linux-only, and 32-bit x86 effectively unsupported. Any eBPF must sit behind a build tag and can never be the only path |
| The injected VPN sidecar is already `Privileged: true` with NET_ADMIN/NET_RAW | `pkg/inject/container.go` | Privileges are **not** a blocker in the sidecar — but eBPF buys no reduction there either |
| The traffic manager runs `Privileged: false` with an empty capability list | `pkg/handler/traffmgr_resources.go` | eBPF here means breaking a clean security posture (PSA restricted, managed clusters) |
| Fargate/Service mode exists precisely because some environments have no NET_ADMIN and no iptables | `pkg/core/udp_reverse_bridge.go`, [06-fargate-mode.md](06-fargate-mode.md) | eBPF can only ever be a **third** branch beside iptables and Fargate, never a replacement for both |
| Batched TUN I/O already landed | `pkg/tun/batch.go` (`BatchSize`, `ReadPackets`) | The cheap datapath win is already taken |
| `vishvananda/netlink` is already a dependency | `go.mod` | Attaching a tc program needs no new Go dependency |

Measured on a current dev box (kernel 7.0.14, BTF present at `/sys/kernel/btf/vmlinux`):
`clang`, `llvm-strip` and `bpftool` are all **absent**, and `/sys/fs/bpf` is not mounted. The
toolchain gap is real work, not a footnote — it lands on every dev machine and on CI.

The runtime image is `debian:bookworm-slim` with the `iptables` package added (`build/Dockerfile`).

## 3. Candidate 1 — Datapath acceleration (XDP / AF_XDP / sockmap)

**Verdict: no. The bottleneck is not in the kernel.**

- Cross-client throughput was diagnosed in [50-crossclient-throughput-diagnosis.md](50-crossclient-throughput-diagnosis.md):
  the limit is B's **single shared inter-client gvisor stack** plus a **drop-if-full receive with
  no backpressure**. Both are userspace design. Making packet delivery to that stack cheaper
  does not raise a ceiling imposed by dropping packets at a full channel.
- For ordinary outbound traffic the dominant cost is the **single TCP tunnel through the
  kubectl port-forward / SPDY path** (or the SSH bypass, [49-ssh-dataplane-bypass.md](49-ssh-dataplane-bypass.md)),
  not TUN read/write syscalls — and the syscall side is already batched (`pkg/tun/batch.go`).
- The clients where this would run are overwhelmingly macOS and Windows. A Linux-only
  acceleration path serves the smallest slice of users for the largest share of the work.

Fix the shared stack and the missing backpressure first. If that is ever done and the datapath
is *then* measured to be syscall-bound on Linux, this becomes worth re-opening — but not before
there is a measurement that says so.

## 4. Candidate 2 — tc-BPF instead of the sidecar's iptables DNAT

**Verdict: the only candidate with genuine appeal. Still no, today.**

The pain is real and visible. The VPN sidecar bootstraps with
(`pkg/inject/container.go`):

```sh
for b in iptables-nft iptables-legacy; do "$b" -t nat -L &>/dev/null && update-alternatives --set iptables /usr/sbin/$b && ... done
iptables  -t nat -A PREROUTING -p tcp ! -s 127.0.0.1 ! -d ${CIDR4} -j DNAT --to :15006
ip6tables -t nat -A PREROUTING -p tcp ! -s ::1       ! -d ${CIDR6} -j DNAT --to :15006
```

That loop is guessing which iptables backend the node's kernel and the image agree on, and the
rule needs a working `nat` table. A tc-BPF classifier doing the redirect would remove the guess,
drop the `iptables` package from the image, and work the same on nft-only hosts.

Against it:

- **It buys no privilege reduction.** The sidecar is already privileged; that is why the
  "eBPF is more secure" argument does not apply here.
- **A kernel floor appears where there is none today.** iptables works on anything; tc-BPF
  redirect needs a modern enough kernel, and KubeVPN does not control the node.
- **A clang/LLVM toolchain plus per-architecture objects** enters the build, for both amd64 and
  arm64 images. The objects can be `go:embed`-ed so nothing extra is needed at *runtime* — but
  CI and every contributor pay at build time.
- **A third datapath branch to maintain**, beside iptables and Fargate, each needing its own
  tests. [06-fargate-mode.md](06-fargate-mode.md) already shows what a second branch costs.

**Reopen if any of these becomes true:** the backend-detection loop causes a field failure; a
target environment ships without a usable `nat` table; or a requirement appears for redirection
decisions that iptables cannot express (per-connection or L7-dependent). None hold today.

## 5. Candidate 3 — Kernel-level observability

**Verdict: no — wrong platform, and the cheap fix already worked.**

Every outage in this family was diagnosed, or missed, on the **client**:
[47-portforward-blackhole-liveness.md](47-portforward-blackhole-liveness.md),
[51-idle-client-route-registration.md](51-idle-client-route-registration.md),
[52-stale-route-src-after-ip-change.md](52-stale-route-src-after-ip-change.md). Those clients
were macOS. eBPF cannot see them.

The Linux side eBPF *could* instrument is the traffic manager, which runs unprivileged by
design. And the measure that actually broke the 20-hour outage open was far cheaper: six
silent failure paths were given throttled warnings (`51` §3.3). That is portable, costs
nothing, and is already in place.

The one honest gap eBPF would close — "the kernel dropped this packet and nobody can say why" —
has not been the failure mode in any incident so far. The failures were decisions made in
*our* code with no log line attached.

## 6. Candidate 4 — Node-level eBPF (CNI-style)

**Verdict: out of scope.** KubeVPN deliberately does not own the node; it installs a workload
and a local TUN. Loading node-level BPF would put it in the CNI's territory, require node
access it never asks for today, and break on every managed cluster that does not grant it.

## 7. If It Is Ever Done, The Minimal Shape

So a future reversal does not start from a blank page:

- `pkg/ebpf`, entirely behind `//go:build linux && ebpf`, with a no-op stub for every other
  platform — matching how `pkg/tun` already splits per OS.
- `github.com/cilium/ebpf` for load/attach (pure Go, survives `CGO_ENABLED=0`);
  `vishvananda/netlink`, already vendored, for the tc qdisc/filter.
- `bpf2go` at build time; commit the generated objects **or** gate them behind a CI job, and
  `go:embed` them so the runtime image needs no toolchain.
- **A runtime feature probe with an iptables fallback is mandatory, not optional.** The node
  kernel is not ours to choose, and a verifier rejection must degrade to the existing path
  rather than fail the injection.
- Tests must cover the fallback, not just the happy path — that is where a third branch rots.

## 8. Measured vs Reasoned

Stated explicitly, because an earlier document in this series asserted kernel behaviour from
code reading and was wrong twice (see [52-stale-route-src-after-ip-change.md](52-stale-route-src-after-ip-change.md) §3.2).

| Claim | Basis |
|---|---|
| No eBPF anywhere in the tree | Measured (grep; the only hit is an embedded gzip blob in `pkg/syncthing/auto`) |
| `CGO_ENABLED=0`, 8 targets including linux/386 | Measured (`Makefile`) |
| Sidecar privileged; traffic manager not | Measured (source) |
| `clang`/`llvm-strip`/`bpftool` absent, `/sys/fs/bpf` unmounted, BTF present | Measured on one dev box; **not** a survey of contributor machines or CI images |
| Cross-client bottleneck is the shared stack + drop-if-full | Measured previously, see `50` |
| Outbound cost is dominated by the single tunnel rather than TUN syscalls | **Reasoned, not measured.** No profile has been taken. It is the load-bearing assumption under §3 — take a profile before overturning that section |
| tc-BPF redirect would work on a modern kernel in a privileged sidecar | Reasoned from the documented feature set; **no prototype was built** |
| Client population is mostly macOS/Windows | Assumption from the product's purpose; no telemetry exists to confirm it |
