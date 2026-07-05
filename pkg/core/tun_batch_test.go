package core

import (
	"bytes"
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// fakeBatchTUN implements batchTUN: it delivers one seeded batch of IP packets, then blocks
// on gate until the test cancels the context (mimicking a TUN with no further traffic).
type fakeBatchTUN struct {
	batch   int
	packets [][]byte
	gate    chan struct{}

	mu     sync.Mutex
	served bool
}

func (f *fakeBatchTUN) BatchSize() int { return f.batch }

func (f *fakeBatchTUN) ReadPackets(bufs [][]byte, sizes []int, off int) (int, error) {
	f.mu.Lock()
	served := f.served
	f.served = true
	f.mu.Unlock()
	if !served {
		n := 0
		for i, p := range f.packets {
			if i >= len(bufs) {
				break
			}
			copy(bufs[i][off:], p)
			sizes[i] = len(p)
			n++
		}
		return n, nil
	}
	<-f.gate
	return 0, context.Canceled
}

// TestPumpTunBatchDispatchesEachSegment verifies that a single batched read yielding multiple
// packets (the GRO/segment-aware case) parses and dispatches each packet individually into the
// canonical [2 len][1 type][IP] layout, with the IP at buf[tunReserve:].
func TestPumpTunBatchDispatchesEachSegment(t *testing.T) {
	src := net.ParseIP("198.18.0.1")
	dst := net.ParseIP("10.0.0.1")
	p1 := buildIPv4Packet(src, dst, []byte("one"))
	p2 := buildIPv4Packet(src, dst, []byte("two"))
	p3 := buildIPv4Packet(src, dst, []byte("three"))

	fake := &fakeBatchTUN{batch: 8, packets: [][]byte{p1, p2, p3}, gate: make(chan struct{})}
	d := &tunDevice{errChan: make(chan error, 1)}

	var mu sync.Mutex
	var got [][]byte
	done := make(chan struct{})
	dispatch := func(buf []byte, n int, s, dd net.IP) {
		cp := make([]byte, n)
		copy(cp, buf[tunReserve:tunReserve+n])
		config.LPool.Put(buf[:]) // dispatch owns buf and returns it to the pool
		mu.Lock()
		got = append(got, cp)
		count := len(got)
		mu.Unlock()
		if count == 3 {
			close(done)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	go d.pumpTunBatch(ctx, "test", fake, dispatch)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for 3 dispatched packets")
	}
	cancel()
	close(fake.gate)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("dispatched %d packets, want 3", len(got))
	}
	for i, want := range [][]byte{p1, p2, p3} {
		if !bytes.Equal(got[i], want) {
			t.Errorf("packet %d mismatch:\n got  %v\n want %v", i, got[i], want)
		}
	}
}
