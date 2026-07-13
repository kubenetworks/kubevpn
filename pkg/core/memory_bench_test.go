package core

import (
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

// TestPacketChannelWorstCaseMemory pins the memory cost of a saturated packet channel:
// a full channel holds MaxSize *Packet, each owning a LargeBufferSize (64 KiB) pooled
// buffer, so one channel's worst case is MaxSize * 64 KiB. The data plane has 7+ such
// channels (tun in/out, gvisor inbound, inter-client, per-slot × ConnPoolSize, route,
// buffered-tcp), so an oversized MaxSize multiplies straight into client RSS.
//
// This is a guardrail (not a micro-benchmark): keep a single channel's worst case
// bounded so a future MaxSize bump cannot silently blow up client memory.
func TestPacketChannelWorstCaseMemory(t *testing.T) {
	worst := MaxSize * config.LargeBufferSize
	t.Logf("worst-case memory per saturated packet channel: MaxSize=%d × %d B = %d MiB",
		MaxSize, config.LargeBufferSize, worst/1024/1024)

	const perChannelCeiling = 32 * 1024 * 1024 // 32 MiB
	if worst > perChannelCeiling {
		t.Fatalf("per-channel worst case %d MiB exceeds the %d MiB ceiling; MaxSize=%d is too large "+
			"(7+ such channels exist — this multiplies into client memory)",
			worst/1024/1024, perChannelCeiling/1024/1024, MaxSize)
	}
}

// TestPacketChannelMemory_Measured reports the actual heap held by a saturated packet
// channel and compares it to the theoretical MaxSize × 64 KiB. Informational (no hard
// assertion — HeapInuse deltas are noisy), it confirms the guardrail's arithmetic
// reflects real resident memory.
func TestPacketChannelMemory_Measured(t *testing.T) {
	ch := make(chan *Packet, MaxSize)
	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)

	for i := 0; i < MaxSize; i++ {
		ch <- &Packet{data: make([]byte, config.LargeBufferSize)}
	}

	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	runtime.KeepAlive(ch)

	delta := int64(m1.HeapInuse) - int64(m0.HeapInuse)
	t.Logf("measured HeapInuse for a saturated channel (depth=%d): ~%d MiB (theoretical %d MiB)",
		MaxSize, delta/1024/1024, (MaxSize*config.LargeBufferSize)/1024/1024)
}
