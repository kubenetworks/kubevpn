# Cross-Client (A→B) Throughput Diagnosis

## Symptom

Two kubevpn clients; client **A** accessing a service on client **B**'s machine (A → B's TUN IP)
is **slow**, and it is slow on **both** data-plane transports:

- `kubectl port-forward` (SPDY/WebSocket via the API server)
- the SSH data-plane bypass (`--ssh-addr` → ClusterIP, see [49](49-ssh-dataplane-bypass.md))

Increasing API-server bandwidth does not help.

## Why "both transports are slow" is the key clue

The SSH bypass has proper TCP window scaling and no SPDY/API-server proxy in the path. If it were
a transport-bandwidth problem, SSH would be fast. Since **both** are slow, the bottleneck is in
the stage the two modes **share** — the cross-client path itself, not the transport.

## The cross-client data path (A→B)

```
A: app → kernel TCP → TUN → routeOutbound → tunInbound
   → runConnPool: flowHash(5-tuple) picks ONE of A's 4 pool slots (tun_client.go)
   → connSlot.writeToConn → transport(SPDY|SSH) → TM:10801
TM: readFromTCPConnWriteToEndpoint → AddRoute(A); HasRoute(B)?
   → RouteHub.WriteToRoutePacket(B)  (hairpin, raw forward — NOT injected into TM's stack)
   → transport → B
B: connSlot.readFromConn (conn_slot.go)
   → trySendToSlot(interClientInbound)  ← DROP-IF-FULL, non-blocking
   → handleGvisorPacket: ONE shared inter-client gvisor stack (gvisor_local_tcp_handler.go)
     → NewLocalStack (userspace TCP, buffers capped ≤4MB, gvisor_stack.go)
     → LocalTCPForwarder → dial B's 127.0.0.1:<port>
Reply (ACKs): B localhost → B's stack → tunInbound → B's pool → TM → WriteToRoute(A) → A
```

Two structural properties of B's receive side stand out:

1. **The inter-client receive channel is drop-if-full (no backpressure).** `connSlot.readFromConn`
   hands inbound cross-client packets to the shared stack via `trySendToSlot` — if the channel is
   full it **silently drops** the packet. This is asymmetric with the OUTBOUND path
   (`runConnPool`), which deliberately **blocks** to apply TCP backpressure and avoid drops. A
   dropped in-order TCP segment forces the sender A into retransmit/RTO.
2. **All cross-client flows funnel through ONE shared inter-client gvisor stack** on B (a single
   `interClientInbound` channel, one `handleGvisorPacket`). Concurrency cannot widen it.

## Measurement (in-process, no cluster, no TUN, no root)

`pkg/core/xclient_throughput_test.go` (`BenchmarkCrossClientThroughput`) wires the real data plane
end-to-end over a clean loopback transport: originator stack A → real `tunDevice` + connection pool
→ tunnel server (RouteHub + `GvisorLocalTCPHandler`) → registered client B → B's inter-client
stack → loopback sink. It compares **A→B (crossclient)** vs **A→cluster** at 1 and 8 flows, on the
**same** transport, and reports the `InterClientDrops` delta.

Run:

```
go test ./pkg/core/ -run '^$' -bench BenchmarkCrossClientThroughput -benchmem -benchtime=2s -count=2
```

Representative results (arm64, clean loopback — transport identical for both columns):

| Path | flows=1 | flows=8 | drops @ f=8 |
|---|---|---|---|
| **A→B (crossclient)** | ~160–180 MB/s | **~73–106 MB/s (drops as concurrency rises)** | **~2900** |
| A→cluster | ~245–255 MB/s | ~348–368 MB/s (scales up) | **0** |

Findings:

- **Cross-client is ~30–45% slower at a single flow, on identical transport** → the gap is
  intrinsic to the cross-client stage, not the transport. This is exactly why *both* port-forward
  and SSH are slow.
- **Cross-client gets *worse* with concurrency (180→~90 MB/s) while cluster scales up
  (255→~360 MB/s).** The single shared inter-client stack + drop-if-full channel is a
  serialization + loss point that concurrency only aggravates.
- **`InterClientDrops` grows from ~100–300 (1 flow) to ~2900 (8 flows); cluster is always 0.**
  The receive side is losing packets — silent TCP segment loss that triggers retransmit/RTO.

> Note: a faithful high-RTT / BDP dimension is **not** modeled in the benchmark. `shapedConn`
> adds a uniform per-read sleep that penalizes a single flow far beyond a real RTT and collapses
> both paths equally (a modeling artifact). Real-RTT throughput belongs in the CI e2e job.

## Ranked bottlenecks (from the measured data)

1. **Receive-side drop-if-full on B (`conn_slot.go`, `trySendToSlot(interClientInbound)`)** —
   confirmed active loss (`InterClientDrops`), grows with load, absent on the cluster path.
   Asymmetric with the blocking-backpressure send path. Prime suspect.
2. **Single shared inter-client gvisor stack on B** — one stack/one channel for all cross-client
   flows; does not scale with concurrency (throughput drops as flows rise). Serialization point.
3. **Single-flow pinned to one pool slot** (`flowHash`) — a single flow cannot use pool
   parallelism; on port-forward all slots share one SPDY connection anyway. (Contributor, not the
   dominant term in these clean-loopback numbers.)
4. **gvisor userspace TCP window cap (≤4MB)** vs the multi-hop BDP of a real A↔TM↔B path — to be
   quantified in the CI e2e (real RTT), not visible on loopback.

## Fix applied — direct injection (bottleneck #1)

The receive path was rebuilt to **mirror the server's cluster path**. The server
(`readFromTCPConnWriteToEndpoint`, gvisor_tun_endpoint.go) reads a packet and calls
`cs.endpoint.InjectInbound` **directly** — no intermediate queue, flow-controlled by TCP's receive
window. B's inter-client path instead pushed onto a bounded Go channel with drop-if-full.

Change (`pkg/core`):

- New `interClientStack` type (`gvisor_local_tcp_handler.go`): owns the shared inter-client gvisor
  stack + endpoint + output pump, and exposes `InjectIP(ip)` which calls `endpoint.InjectInbound`.
- `conn_slot.go` `readFromConn`: for `packetTypeToGvisor`, calls `s.interClient.InjectIP(ip)`
  directly (then returns the buffer to the pool) instead of `trySendToSlot(interClientInbound)`.
- `tun_client.go`: the `interClientInbound` channel and the `client-gvisor-inter` routine are gone;
  `runConnPool` creates the shared `interClientStack` before its slots and hands each slot a
  reference.
- The diagnostic `InterClientDrops` counter (`datapath_stats.go`) is removed — the drop path it
  measured no longer exists; flow control is now TCP's receive window.

Why this is safe for liveness: `InjectInbound` delivers synchronously only up to the transport
layer and returns promptly (the localhost `io.Copy` runs in the forwarder's own goroutines), so a
busy stack never blocks a slot's reader. That matters because the periodic heartbeat's echo *reply*
is routed back over a **data** conn (see `handleControlConn`), so a stalled data-slot reader would
otherwise starve the liveness watchdog. A blocking send would have had that hazard; direct
injection does not.

### Verified (same in-process benchmark, arm64, clean loopback)

| Path | flows=1 | flows=8 |
|---|---|---|
| A→B **before** | ~160–180 MB/s | **~73–106 MB/s (regressed with concurrency)** |
| A→B **after** | ~110–226 MB/s | **~210–243 MB/s (scales up)** |
| A→cluster | ~180–226 MB/s | ~366–379 MB/s |

The pathological "more concurrency → lower throughput" is gone; cross-client now scales up with
load and all bytes arrive (no silent loss). Per-op allocations also dropped (~946→~500: the
intermediate channel + per-packet `Packet` are gone). The residual gap to the cluster path at 8
flows is the inherent extra hop (B's single shared stack + the return through B's pool) — bottleneck
#2/#3, far milder than the drop-induced collapse.

## Second finding — per-packet logging (the dominant real-world cost)

After the direct-injection fix was deployed to two real clients (interA/interB), throughput was
still poor. Their daemon logs (`root_daemon.log`) rotated 100MB every few minutes; a 200k-line
sample was **~99.9% per-packet log lines** — 2–4 lines per packet in the data-plane hot path
(`packet.go` `logIPPacket`, gvisor `sniffer.go`, `gvisor_local_tun_endpoint.go`). Every packet was
being formatted and written to the log synchronously.

Root cause:

1. The daemon's per-RPC logger records the file at Debug **by design** (a full control-plane
   record — see `log.IsDebugEnabled`), so `logIPPacket`, gated on `IsDebugEnabled(ctx)`, fired for
   every packet in the daemon.
2. gvisor's `sniffer.LogPackets` defaults to **1** and was never disabled, so every
   `sniffer.NewWithPrefix`-wrapped stack and every bare `sniffer.LogPacket` call logged every packet
   (gvisor glog Info → the always-Debug plog target; lowering plog to Info would not stop Info).

Per-packet tracing is a hot-path cost that must not ride on the always-on daemon debug record.

### Fix — reference-counted, opt-in packet tracing (off by default)

- `pkg/core/packet_trace.go`: `init()` sets `sniffer.LogPackets.Store(0)` (neutralizes gvisor's
  default per-packet logging in every binary). `AcquirePacketLogging()` turns tracing on and returns
  an idempotent release; a reference count keeps it on while any holder is active and restores off
  when the last releases.
- `packet.go` `logIPPacket`: gated on `packetLoggingEnabled()` (the toggle) instead of
  `IsDebugEnabled(ctx)` — decoupled from the daemon's always-Debug file record; its `ctx` parameter
  was dropped as no longer needed.
- Wiring reuses the existing `--debug` intent (no new CLI flag, no proto change): the root daemon's
  `Connect` handler (`daemon/action/connect.go`) acquires when `req.Level == Debug` and releases via
  a `DataSession` rollback (scoped to the connection's lifetime, ref-counted across `--debug`
  connections); `kubevpn server` (traffic-manager pod) acquires once for the process under `--debug`.

Effect: without `--debug`, per-packet logging is fully off and the hot path pays only an atomic
load (`BenchmarkLogIPPacket`: **1.1 ns/op off vs ~1676 ns/op on** — a ~1500× per-packet cost). With
`connect --debug`, tracing turns on for that connection and off again when it ends.

## Remaining work

- **CI minikube e2e** (real RTT): `iperf3` A→B single-stream vs `-P 8`, and A→cluster, on both
  transports; sample gvisor send window to quantify bottleneck #4 (4MB window vs BDP).
- **Bottleneck #2/#3** (only if the e2e shows the residual gap matters under real RTT): shard B's
  inter-client stack per flow / right-size the gvisor buffers, and revisit single-flow slot pinning.

## Related docs

- [01-network-architecture.md](01-network-architecture.md) — full data path, connection pool, MTU
- [47-portforward-blackhole-liveness.md](47-portforward-blackhole-liveness.md) — port-forward instability
- [48-shared-server-gvisor-stack.md](48-shared-server-gvisor-stack.md) — server-side stack model
- [49-ssh-dataplane-bypass.md](49-ssh-dataplane-bypass.md) — SSH transport that also shows the symptom
