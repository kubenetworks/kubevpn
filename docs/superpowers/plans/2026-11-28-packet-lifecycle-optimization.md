# Packet Lifecycle Optimization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reduce memory held by known-size tunnel packet construction while preserving the existing ownership protocol, unknown-size reads, throughput, and shutdown behavior.

**Architecture:** Add 4 KiB and 16 KiB pools beside the existing 64 KiB `config.LPool`. `newPacketFromPool` selects the smallest containing class; `release` returns storage to the matching class and drops oversized storage. Only the known-size paths are changed; unknown-size TUN/socket reads keep `config.LPool`.

**Tech Stack:** Go 1.26+, `sync.Pool`, gVisor PacketBuffer/View, Kubernetes/GitHub real-cluster integration tests.

**Approved spec:** `docs/superpowers/specs/2026-11-28-packet-lifecycle-optimization-design.md`

## Global Constraints

- Start on `perf/packet-lifecycle` at spec commit `76a7db19d`; make one logical commit per implementation task.
- Preserve `acquire`/`release` semantics, including `refs == -1` on final release. Do not add finalizers or lifecycle goroutines.
- Do not change `MaxSize`, channel admission, framing, heartbeat, route registration, or gVisor injection ownership.
- Unknown-size paths (`pumpTunSingle`, `pumpTunBatchWithPool`, and `packetReader.read`) keep the existing 64 KiB `config.LPool` behavior.
- Class boundaries are 4 KiB, 16 KiB, and 64 KiB. Buffers larger than 64 KiB are standalone and are not returned to any pool.
- Use `len(data)` for recycling class selection, not `cap(data)`, because Linux `config.LPool` deliberately has two extra capacity bytes.
- No CLI/RPC/protobuf changes, no production per-packet metrics lock or per-packet log, and no `MaxSize` tuning in this project.
- Write tests before production behavior and observe the expected failure before implementing. Fake tests must not require a cluster.
- Before every commit run `go build ./...`, `go vet ./pkg/...`, `go test ./pkg/...`, `make ut-compile`, and `git diff --check`.
- Final acceptance requires real-TUN and full GitHub Test, Coverage, Node-Boundary PoC, and two-node matrices. Linux jobs over 40 minutes require investigation.

---

### Task 1: Add Size-Class Packet Buffer Pools

**Files:**
- Modify: `pkg/tunnel/packet.go`
- Create: `pkg/tunnel/packet_buffer_test.go`
- Modify: `pkg/tunnel/memory_bench_test.go`

**Interfaces:**
- `newPacketFromPool(size int) *packet` keeps its signature.
- `release` keeps its signature and implicit-owner semantics.
- New unexported production constants: `smallPacketBufferSize = 4 * 1024`, `mediumPacketBufferSize = 16 * 1024`.
- Test-only expected-size constants: `testSmallBufferSize = 4 * 1024`, `testMediumBufferSize = 16 * 1024`.

- [ ] **Step 1: Add failing size-class and lifecycle tests**

Create `pkg/tunnel/packet_buffer_test.go` with these tests:

```go
package tunnel

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

const (
	testSmallBufferSize  = 4 * 1024
	testMediumBufferSize = 16 * 1024
)

func TestNewPacketFromPoolUsesSmallestContainingClass(t *testing.T) {
	tests := []struct {
		size int
		want int
	}{
		{0, testSmallBufferSize},
		{1, testSmallBufferSize},
		{testSmallBufferSize, testSmallBufferSize},
		{testSmallBufferSize + 1, testMediumBufferSize},
		{testMediumBufferSize, testMediumBufferSize},
		{testMediumBufferSize + 1, config.LargeBufferSize},
		{config.LargeBufferSize, config.LargeBufferSize},
		{config.LargeBufferSize + 1, config.LargeBufferSize + 1},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("size=%d", tt.size), func(t *testing.T) {
			packet := newPacketFromPool(tt.size)
			if len(packet.data) != tt.want {
				t.Fatalf("len(data)=%d, want %d", len(packet.data), tt.want)
			}
			packet.release()
		})
	}
}

func TestUnknownReadBufferStillUsesLargePool(t *testing.T) {
	packet := newPacketFromPool(config.LargeBufferSize)
	if len(packet.data) != config.LargeBufferSize {
		t.Fatalf("len(data)=%d, want %d", len(packet.data), config.LargeBufferSize)
	}
	packet.release()
}

func TestBufferedTCPWriteUsesActualFrameClass(t *testing.T) {
	conn := &bufferedTCP{ch: make(chan *packet, 1), done: make(chan struct{})}
	frame := bytes.Repeat([]byte{0xab}, 1500)
	if _, err := conn.Write(frame); err != nil {
		t.Fatal(err)
	}
	packet := <-conn.ch
	defer packet.release()
	if len(packet.data) != testSmallBufferSize {
		t.Fatalf("1500-byte frame used %d-byte buffer, want %d", len(packet.data), testSmallBufferSize)
	}
	if !bytes.Equal(packet.frame(), frame) {
		t.Fatal("frame contents changed")
	}
}
```

Add `fmt` to the imports. Do not add a real network connection: the one-element channel accepts the packet without using `conn.Conn` or the diagnostic wait stage. The test exercises the real `Write` copy and ownership transfer. The test-only constants are shared by all test files in package `tunnel`; production constants are introduced only in Step 4.

- [ ] **Step 2: Add the allocation-size benchmark**

Append to `pkg/tunnel/memory_bench_test.go`:

```go
func BenchmarkNewPacketFromPoolSize(b *testing.B) {
	for _, tc := range []struct {
		name string
		size int
		want int
	}{
		{name: "mtu", size: 1500, want: testSmallBufferSize},
		{name: "medium", size: 12000, want: testMediumBufferSize},
		{name: "large", size: 65000, want: config.LargeBufferSize},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(float64(tc.want), "buffer_B")
			for i := 0; i < b.N; i++ {
				packet := newPacketFromPool(tc.size)
				packet.release()
			}
		})
	}
}
```

- [ ] **Step 3: Verify RED and record the baseline**

Run:

```bash
gofmt -w pkg/tunnel/packet_buffer_test.go pkg/tunnel/memory_bench_test.go
go test ./pkg/tunnel -run 'TestNewPacketFromPoolUsesSmallestContainingClass|TestUnknownReadBufferStillUsesLargePool|TestBufferedTCPWriteUsesActualFrameClass' -count=1
go test ./pkg/tunnel -run '^$' -bench '^BenchmarkNewPacketFromPoolSize$' -benchmem -benchtime=2s -count=2
```

Expected: class tests fail on the old implementation because known-size frames still use a 64 KiB buffer. The benchmark compiles and records the old allocation behavior before production code changes. Save the baseline benchmark output in the task report.

- [ ] **Step 4: Implement class allocation and recycling**

In `pkg/tunnel/packet.go`, add:

```go
const (
	smallPacketBufferSize  = 4 * 1024
	mediumPacketBufferSize = 16 * 1024
)

var smallPacketBufferPool = sync.Pool{New: func() any {
	return make([]byte, smallPacketBufferSize)
}}
var mediumPacketBufferPool = sync.Pool{New: func() any {
	return make([]byte, mediumPacketBufferSize)
}}
```

Replace `newPacketFromPool` with:

```go
func newPacketFromPool(size int) *packet {
	switch {
	case size <= smallPacketBufferSize:
		return newPacket(smallPacketBufferPool.Get().([]byte), 0, nil, nil)
	case size <= mediumPacketBufferSize:
		return newPacket(mediumPacketBufferPool.Get().([]byte), 0, nil, nil)
	case size <= config.LargeBufferSize:
		return newPacket(packetBufferFromPool(), 0, nil, nil)
	default:
		return newPacket(make([]byte, size), 0, nil, nil)
	}
}
```

In `release`, replace the unconditional `config.LPool.Put(data[:])` with:

```go
switch len(data) {
case smallPacketBufferSize:
	smallPacketBufferPool.Put(data[:])
case mediumPacketBufferSize:
	mediumPacketBufferPool.Put(data[:])
case config.LargeBufferSize:
	config.LPool.Put(data[:])
}
```

There is intentionally no default case: oversized and noncanonical storage is dropped. Do not change `packetBufferFromPool`, `newPacket`, `acquire`, reference values, or metadata clearing.

- [ ] **Step 5: Verify GREEN and full verification**

Run:

```bash
gofmt -w pkg/tunnel/packet.go pkg/tunnel/packet_buffer_test.go pkg/tunnel/memory_bench_test.go
go test ./pkg/tunnel -run 'TestNewPacketFromPoolUsesSmallestContainingClass|TestUnknownReadBufferStillUsesLargePool|TestBufferedTCPWriteUsesActualFrameClass|TestPacketRefcountCharacterization|TestPacketPoolResetsMetadata|TestPacketOwnershipFloodIntegration' -count=1
go test ./pkg/tunnel -run '^$' -bench '^BenchmarkNewPacketFromPoolSize$' -benchmem -benchtime=2s -count=2
go build ./...
go vet ./pkg/...
go test ./pkg/...
make ut-compile
git diff --check
```

Expected: the class-boundary and ownership tests pass. The benchmark reports 4096, 16384, and 65536 `buffer_B` for the three sizes. Compare allocations and runtime with the baseline rather than assuming them better.

Commit:

```bash
git add pkg/tunnel/packet.go pkg/tunnel/packet_buffer_test.go pkg/tunnel/memory_bench_test.go
git commit -m "perf: use size-class packet buffer pools"
```

---

### Task 2: Size gVisor Output Buffers by Actual Packet Size

**Files:**
- Modify: `pkg/tunnel/packet.go`
- Modify: `pkg/tunnel/refactor_test.go`
- Modify: `pkg/tunnel/memory_bench_test.go`
- Modify: `docs/18-gvisor-network-stack.md`
- Modify: `docs/22-tun-device.md`

**Interfaces:**
- `copyPacketToPool(pkt *stack.PacketBuffer, prefix byte, headroom int) *packet` keeps its signature and consumes `pkt` exactly once.
- The class size is `headroom + typePrefixLen + pkt.Size()` before `pkt.DecRef()`.

- [ ] **Step 1: Add failing actual-size tests**

Add `TestCopyPacketToPoolUsesActualSizeClass` to `pkg/tunnel/refactor_test.go`:

```go
func TestCopyPacketToPoolUsesActualSizeClass(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload int
		want    int
	}{
		{name: "mtu", payload: 1500, want: testSmallBufferSize},
		{name: "medium", payload: 12000, want: testMediumBufferSize},
		{name: "large", payload: 65000, want: config.LargeBufferSize},
		{name: "standalone", payload: 65534, want: 65537},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte{0x5a}, tc.payload)
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(payload)})
			packet := copyPacketToPool(pkt, packetTypeToTUN, datagramHeaderLen)
			defer packet.release()
			if len(packet.data) != tc.want {
				t.Fatalf("len(data)=%d, want %d", len(packet.data), tc.want)
			}
			if !bytes.Equal(packet.ip(), payload) {
				t.Fatal("payload changed during size-class copy")
			}
		})
	}
}
```

Add `bytes` to the imports. The standalone case verifies `datagramHeaderLen + typePrefixLen + 65534 == 65537`, so it is not retained in a class pool.

Add `BenchmarkCopyPacketToPoolMTU` to `memory_bench_test.go`:

```go
func BenchmarkCopyPacketToPoolMTU(b *testing.B) {
	payload := make([]byte, 1500)
	b.ReportAllocs()
	b.ReportMetric(float64(testSmallBufferSize), "buffer_B")
	for i := 0; i < b.N; i++ {
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(payload)})
		packet := copyPacketToPool(pkt, packetTypeToTUN, datagramHeaderLen)
		packet.release()
	}
}
```

Add the gVisor `buffer` and `stack` imports to `memory_bench_test.go`.

- [ ] **Step 2: Verify RED**

Run:

```bash
gofmt -w pkg/tunnel/refactor_test.go pkg/tunnel/memory_bench_test.go
go test ./pkg/tunnel -run '^TestCopyPacketToPoolUsesActualSizeClass$' -count=1
go test ./pkg/tunnel -run '^$' -bench '^BenchmarkCopyPacketToPoolMTU$' -benchmem -benchtime=2s -count=2
```

Expected: every actual-size case still uses a 64 KiB buffer, so the MTU and medium cases fail.

- [ ] **Step 3: Use the actual gVisor packet size**

In `copyPacketToPool`, replace only the constructor argument:

```go
packet := newPacketFromPool(headroom + typePrefixLen + pkt.Size())
```

Keep `pkt.AsSlices()` copying and `pkt.DecRef()` unchanged. Do not move size calculation after `DecRef`.

- [ ] **Step 4: Update lifecycle documentation**

In `docs/18-gvisor-network-stack.md` §7.4, document that known-size construction uses the smallest of 4/16/64 KiB, while unknown-size `packetReader` reads retain 64 KiB. State that `copyPacketToPool` uses `headroom + typePrefixLen + pkt.Size()` and that oversized storage is not pooled.

In `docs/22-tun-device.md`, explicitly state that single and batched TUN reads retain their current buffer sizes and are not part of the class-based optimization.

- [ ] **Step 5: Verify GREEN, benchmarks, and full verification**

Run:

```bash
gofmt -w pkg/tunnel/packet.go pkg/tunnel/refactor_test.go pkg/tunnel/memory_bench_test.go
go test ./pkg/tunnel -run 'TestCopyPacketToPool|TestPacketOwnershipFloodIntegration|TestReadFromConn_InjectsInterClientToSharedStack' -count=1
go test ./pkg/tunnel -run '^$' -bench '^(BenchmarkNewPacketFromPoolSize|BenchmarkCopyPacketToPoolMTU)$' -benchmem -benchtime=2s -count=2
go test ./pkg/tunnel -run '^$' -bench '^BenchmarkCrossClientThroughput$' -benchmem -benchtime=2s -count=2
go build ./...
go vet ./pkg/...
go test ./pkg/...
make ut-compile
git diff --check
```

Expected: actual-size and ownership tests pass. MTU copy reports 4096 `buffer_B`; cross-client has no multi-flow collapse. Record benchmark results and do not claim a throughput improvement without evidence.

Commit:

```bash
git add pkg/tunnel/packet.go pkg/tunnel/refactor_test.go pkg/tunnel/memory_bench_test.go docs/18-gvisor-network-stack.md docs/22-tun-device.md
git commit -m "perf: size gvisor output buffers by packet size"
```

---

## Final Verification Gate

- [ ] Re-run the complete local loop and lifecycle-focused integration tests:

```bash
go test ./pkg/tunnel -run 'TestPacketOwnershipFloodIntegration|TestReadFromConn_InjectsInterClientToSharedStack|TestCopyPacketToPool|TestBufferedTCPWriteUsesActualFrameClass' -count=1
go build ./...
go vet ./pkg/...
go test ./pkg/...
make ut-compile
```

- [ ] Confirm invariants by search and review:
  - `newPacketFromPool` is the only class-selection constructor.
  - `release` uses `len(data)` and has no oversized pool fallback.
  - `pumpTunSingle`, `pumpTunBatchWithPool`, and `packetReader.read` are unchanged.
  - `copyPacketToPool` still calls `pkt.DecRef()` exactly once.
  - `MaxSize` is unchanged.

- [ ] Run real-TUN tests on demand:

```bash
sudo -E env PATH="$PATH" make ut-tun
```

- [ ] Push the branch and run the complete Test matrix including `linux-multinode`, Coverage, and Node-Boundary PoC. Do not use targeted suites as final evidence.

- [ ] Compare the before/after benchmark records. Complete only if the memory-size assertions and correctness tests pass without statistically significant throughput regression. Do not open queue-depth or gVisor View redesign work in this branch.
