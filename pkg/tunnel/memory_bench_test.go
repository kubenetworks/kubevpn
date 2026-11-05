package tunnel

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// BenchmarkLPoolGetPut measures the cost of borrowing/returning a large I/O buffer.
// It should be near-zero-alloc after warmup — proving that packet buffers are pooled
// and that steady-state memory is driven by how many buffers are pinned in-flight
// (i.e. by channel depth), not by per-packet allocation.
func BenchmarkLPoolGetPut(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf := config.LPool.Get().([]byte)
		config.LPool.Put(buf[:])
	}
}

// TestPacketChannelWorstCaseMemory bounds distinct backing allocations in a
// saturated channel using the actual factory capacity and large-object rounding.
// Linux capacity 65538 rounds to 72 KiB, not 64 KiB. Multiple queues can pin many
// distinct buffers, but shared packet references must not be counted twice.
//
// This is a guardrail (not a micro-benchmark): keep a single channel's worst case
// bounded so a future MaxSize bump cannot silently blow up client memory.
func TestPacketChannelWorstCaseMemory(t *testing.T) {
	buf := config.LPool.New().([]byte)
	// Large objects round to runtime heap pages (8 KiB), not slice capacity.
	allocation := (cap(buf) + 8191) / 8192 * 8192
	worst := MaxSize * allocation
	t.Logf("channel backing model: logical=%d capacity=%d rounded=%d × depth=%d = %d bytes",
		len(buf), cap(buf), allocation, MaxSize, worst)

	const perChannelCeiling = 32 * 1024 * 1024 // 32 MiB
	if worst > perChannelCeiling {
		t.Fatalf("per-channel worst case %d MiB exceeds the %d MiB ceiling; MaxSize=%d is too large "+
			"(7+ such channels exist — this multiplies into client memory)",
			worst/1024/1024, perChannelCeiling/1024/1024, MaxSize)
	}
}

// TestPacketChannelMemory_Measured reports heap held by distinct actual-factory
// buffers in a saturated channel. Informational: HeapInuse deltas are noisy and
// are not process RSS.
func TestPacketChannelMemory_Measured(t *testing.T) {
	ch := make(chan *packet, MaxSize)
	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)

	for i := 0; i < MaxSize; i++ {
		ch <- &packet{data: config.LPool.New().([]byte)}
	}

	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	runtime.KeepAlive(ch)

	delta := int64(m1.HeapInuse) - int64(m0.HeapInuse)
	t.Logf("actual LPool factory saturated channel depth=%d: HeapInuse delta=%d bytes (includes allocator rounding)", MaxSize, delta)
}

// Host allocation model only; neither RSS nor a kernel TUN benchmark. Every
// counted queued buffer is distinct: multiple packet references do not multiply
// its backing allocation. Linux's two extra capacity bytes cross a heap page.
func TestStage4HeapAllocationModel(t *testing.T) {
	cost := make(map[int]uint64)
	for _, capacity := range []int{65536, 65538, 65551} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			buffers := make([][]byte, 128)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for i := range buffers {
				buffers[i] = make([]byte, capacity)
			}
			runtime.ReadMemStats(&after)
			runtime.KeepAlive(buffers)
			cost[capacity] = (after.TotalAlloc - before.TotalAlloc) / 128
			t.Logf("distinct backing requested=%d measured allocation=%d bytes/buffer", capacity, cost[capacity])
		})
	}
	removed := 128 * cost[65551]
	t.Logf("removed adapter scratch: 128*65551=%d logical bytes; measured backing=%d bytes", 128*65551, removed)
	for _, queued := range []int{0, 256, 2048} {
		// Pump-owned + queued distinct buffers; scratch exists only in old model.
		old := uint64(128+queued)*cost[65536] + removed
		fresh := uint64(128+queued) * cost[65538]
		t.Logf("distinct queued=%d old=%d direct=%d delta=%d bytes (shared refs counted once)", queued, old, fresh, int64(fresh)-int64(old))
	}
}
