# Packet Lifecycle Optimization Design

> Date: 2026-11-28
> Base: `master` after PR #824 (`8908df491`)
> Branch: `perf/packet-lifecycle`
> Status: approved for specification review

## 1. Context and Evidence

The tunnel has two distinct packet representations:

1. `pkg/tunnel.packet`: a pooled `[]byte` plus metadata and an atomic reference
   count, used by TUN reads, framing, route queues, and asynchronous writes.
2. `stack.PacketBuffer` with gVisor `buffer.View`: used inside gVisor stacks.

The current tunnel packet model is already deliberately engineered:

- `config.LPool` pools 64 KiB byte slices;
- `packetPool` pools metadata;
- `acquire`/`release` allows asynchronous sharing without copying;
- successful channel sends transfer a reference, while failure paths release it;
- buffered writers batch and drain queued references on errors and shutdown;
- gVisor injection uses `buffer.MakeWithData` to transfer ownership through a
  controlled copy;
- queue depth is bounded by `MaxSize = 256`.

Recent benchmarks on the merged baseline show approximately 200 MB/s for both
cross-client and cluster paths, without multi-flow collapse. Therefore this is
not a protocol rewrite or an attempt to remove the existing gVisor ownership
boundary.

The remaining balanced goals are memory, CPU, and lifecycle maintainability.
The clearest opportunity is that known-size packet construction can pin a 64 KiB
buffer even when the actual frame is MTU-sized. Unknown-size read paths must not
be changed speculatively because `net.Conn.Read` provides no general safe
“read, then grow without truncation” contract.

## 2. Goals

1. Reduce allocation bytes for known-size packet construction while preserving
   throughput and backpressure.
2. Keep lifecycle ownership explicit and testable across normal, shared,
   asynchronous, cancellation, and failure paths.
3. Make buffer-class selection and recycling consistent, with no double put,
   use-after-free, stale metadata, truncation, or frame-format changes.
4. Preserve existing cross-client behavior and avoid statistically significant
   throughput regression.
5. Decide any later queue-depth or gVisor View changes only from new evidence.

## 3. Non-goals

- No replacement of the tunnel packet type with gVisor PacketBuffer/View.
- No new peer-to-peer protocol, eBPF path, or inter-client data-plane redesign.
- No removal of the controlled gVisor injection copy.
- No global `MaxSize` change in the first implementation.
- No shrinking of unknown-size TUN/socket read buffers without a separate safe
  design.
- No production per-packet logging, global metrics lock, or new lifecycle
  finalizer goroutine.
- No CLI, RPC, wire-framing, heartbeat, or route-registration changes.

## 4. Lifecycle Contract

The lifecycle is:

```text
pool Get
  → one owning packet
  → optional acquire() sharing
  → successful channel/queue reference transfer
  → final release()
  → return to the matching pool, or discard oversized storage
```

Required invariants:

1. A byte buffer is recycled at most once. Metadata is cleared on final release
   and must not be accessed afterward.
2. A successful channel send transfers one reference; an unsuccessful send does
   not, so the caller releases it.
3. `done` stops new enqueue admission. It does not prove writer completion;
   `runDone`/owner join remains mandatory.
4. `acquire`/`release` retains the current implicit-owner convention. A plain
   `&packet{}` has one owner; final release is detected at `refs == -1`.
5. gVisor PacketBuffers remain independently owned. Releasing the source tunnel
   packet must not invalidate an injected gVisor packet.
6. Buffers larger than the maximum class are discarded rather than retained in
   a pool.
7. The canonical `[2-byte length][1-byte type][raw IP]` layout and all short
   read/write behavior remain unchanged.

The implementation should centralize size-class allocation and recycling in
`pkg/tunnel/packet.go`. It should not add finalizers or background lifecycle
workers.

## 5. Size-Class Buffer Pools

### 5.1 Classes

Use 4 KiB, 16 KiB, and 64 KiB classes. A constructor selects the smallest class
that can contain the complete requested frame, including all header and prefix
bytes. Requests above 64 KiB allocate standalone storage that is not returned to
a class pool.

The exact class used by a buffer must be discoverable at recycling time without
changing ownership semantics. Implementations may derive it from a canonical
buffer-length/capacity convention or explicit metadata, but must reset any such
metadata when constructing and releasing packets.

`config.LPool` remains available for existing unknown-size paths and other
packages. This project does not replace it globally or change its factory
contract.

### 5.2 Known-size paths eligible for class selection

| Path | Required behavior |
|---|---|
| `bufferedTCP.Write(b)` | Select a class using `len(b)` before constructing the packet |
| `copyPacketToPool` | Select a class using the gVisor packet's actual size plus headroom and type prefix |
| Other constructors with an exact complete frame size | Select the smallest containing class without changing layout |

These paths must calculate size before allocation; selecting a class after the
copy would not reduce retained memory.

### 5.3 Unknown-size paths preserved in this project

- `tunDevice.pumpTunSingle`;
- `packetReader.read`;
- `tunDevice.pumpTunBatchWithPool`.

These paths retain the existing 64 KiB buffers. They must not grow by re-reading
a stream, assume a truncated read is complete, or rely on OS-specific behavior.
A separate design would be required to prove a safe size-aware read strategy.

Because these paths remain unchanged, an MTU-sized known-size construction can
move from 64 KiB to 4 KiB, but total process RSS must not be promised to fall by
the same ratio.

## 6. Queue and Backpressure

The first implementation does not change `MaxSize`, channel ownership, or queue
admission. Any later queue-depth change must be role-specific and based on queue
high-water, latency, and throughput measurements from this work.

Benchmarks may collect frame-size distributions and queue high-water marks, but
production hot paths must not gain a global metrics lock or per-packet log.
Lifecycle counters belong in test allocators and diagnostic fixtures, not as a
mandatory per-packet production tax.

## 7. Implementation Phases

### Phase 1: Characterization and failing tests

- Add construction-size and recycling tests around the three classes and their
  boundaries.
- Add ownership tests for shared references, final release, incorrect-class
  reclamation prevention, and metadata reset.
- Extend cancellation/drain coverage to ensure every reference reaches a
  terminal release exactly once.
- Capture current construction allocation sizes and cross-client baselines.
- Observe the expected failures before changing production code.

### Phase 2: Minimal class-based allocation

- Add the three classes and standalone oversized behavior in `packet.go`.
- Convert only known-size paths listed in §5.2.
- Preserve all unknown-size paths and existing gVisor ownership boundaries.
- Keep comments and documentation aligned with actual behavior.

### Phase 3: Evidence review

- Compare allocation bytes, CPU, cross-client throughput, and lifecycle tests.
- Stop if there is no meaningful improvement or a correctness regression.
- Consider queue tuning or a gVisor View redesign only as a separately approved
  project supported by new measurements.

## 8. Test Matrix

| Area | Required coverage |
|---|---|
| Class boundaries | 4 KiB±1, 16 KiB±1, 64 KiB±1, and requests above 64 KiB |
| Layout | `[2 len][1 type][IP]`, `frameLen`, `writeTo`, and short writes |
| Ownership | Multiple owners, double-release panic, metadata reset, one final recycle |
| Pooling | Correct class on return, no oversized retention, no stale source/destination state |
| Queues | Full-queue backpressure, canceled admission, closed admission, error drain |
| gVisor | Source release and injected-packet independence, correct DecRef |
| Reads | Single TUN, batched TUN, and packetReader behavior remains unchanged |
| Sizes | Empty input, exact class maxima, maximum legal frame, oversized frame, short read/write |
| Integration | Real route queue, buffered writer, cancellation, reconnect, and flood ownership tests |

Tests must assert behavior and lifecycle outcomes, not merely pool counters.
Existing ownership and integration suites remain mandatory.

## 9. Performance and Acceptance Criteria

1. MTU-sized known-size construction uses a 4 KiB buffer rather than 64 KiB.
2. Every class transition uses the smallest containing class and is covered by
   tests.
3. No new double put, use-after-free, stale metadata, truncated frame, or frame
   format change is found.
4. Existing cross-client benchmarks show no statistically significant
   throughput regression. A claim of improvement is not required to accept the
   memory reduction.
5. The following pass:

   ```bash
   go build ./...
   go vet ./pkg/...
   go test ./pkg/...
   make ut-compile
   ```

6. Real-TUN tests and the complete GitHub Test, Coverage, Node-Boundary PoC,
   and two-node matrices pass. Linux jobs exceeding 40 minutes are treated as
   failures requiring investigation.

## 10. Documentation and Rollback

Update `docs/18-gvisor-network-stack.md`, `docs/22-tun-device.md`, and current
packet-lifecycle references to distinguish class-based known-size construction
from unchanged unknown-size reads. The controlled gVisor copy must remain
explicitly documented.

If class-based allocation causes correctness or performance regressions, revert
the allocation change and its new tests as one logical unit. Do not retain
partial size-class behavior in only some of the known-size paths.
