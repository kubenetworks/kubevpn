package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

type writeBatchTUN struct {
	net.Conn
	batch   int
	calls   int
	written [][]byte
	short   bool
}

func (f *writeBatchTUN) BatchSize() int { return f.batch }
func (f *writeBatchTUN) WritePackets(packets [][]byte) (int, error) {
	f.calls++
	if len(packets) > f.batch {
		return 0, fmt.Errorf("batch exceeds device limit")
	}
	for _, p := range packets {
		f.written = append(f.written, append([]byte(nil), p...))
	}
	if f.short {
		return 0, nil
	}
	return len(packets), nil
}
func (f *writeBatchTUN) Write(p []byte) (int, error) {
	f.calls++
	f.written = append(f.written, append([]byte(nil), p...))
	return len(p), nil
}
func TestWriteTunBatch(t *testing.T) {
	for _, bs := range []int{1, 8, 64} {
		t.Run(fmt.Sprint(bs), func(t *testing.T) {
			f := &writeBatchTUN{batch: bs}
			q := make(chan *Packet, 70)
			var packets []*Packet
			for i := 0; i < 70; i++ {
				p := batchPacket(60, i)
				packets = append(packets, p)
				q <- p
			}
			close(q)
			d := &tunDevice{tun: f, tunOutbound: q, errChan: make(chan error, 1)}
			d.writeToTun(context.Background())
			limit := min(bs, 32)
			if f.calls != (70+limit-1)/limit {
				t.Fatalf("calls=%d", f.calls)
			}
			if len(f.written) != 70 {
				t.Fatalf("written=%d", len(f.written))
			}
			for i, p := range packets {
				if !bytes.Equal(f.written[i], p.data[3:60]) {
					t.Fatalf("packet %d corrupt", i)
				}
			}
			checkBatchRefs(t, packets)
		})
	}
}
func TestWriteTunBatchShortCount(t *testing.T) {
	f := &writeBatchTUN{batch: 8, short: true}
	q := make(chan *Packet, 3)
	var packets []*Packet
	for i := 0; i < 3; i++ {
		p := batchPacket(60, i)
		packets = append(packets, p)
		q <- p
	}
	close(q)
	d := &tunDevice{tun: f, tunOutbound: q, errChan: make(chan error, 1)}
	d.writeToTun(context.Background())
	select {
	case err := <-d.errChan:
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatal(err)
		}
	default:
		t.Fatal("short batch not reported")
	}
	if f.calls != 1 {
		t.Fatal("short packet write retried")
	}
	checkBatchRefs(t, packets)
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
