package core

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// TestPumpTun_DispatchesValidSkipsInvalid verifies the shared TUN read loop hands parseable
// packets to dispatch and silently drops (continues past) unparseable ones.
func TestPumpTun_DispatchesValidSkipsInvalid(t *testing.T) {
	tun := newMockTUN()
	d := &tunDevice{tun: tun, errChan: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type got struct {
		n        int
		src, dst net.IP
	}
	dispatched := make(chan got, 4)
	go d.pumpTun(ctx, "[Test]", func(buf []byte, n int, src, dst net.IP) {
		dispatched <- got{n, src, dst}
		config.LPool.Put(buf[:])
	})

	// An unparseable packet (IP version nibble 0) must be dropped, not dispatched or fatal.
	tun.readCh <- make([]byte, 20)
	// A valid IPv4 packet must reach dispatch.
	src, dst := net.IPv4(198, 18, 0, 2), net.IPv4(10, 0, 0, 5)
	ipPkt := buildIPv4Packet(src, dst, []byte("ok"))
	tun.readCh <- ipPkt

	select {
	case g := <-dispatched:
		if g.n != len(ipPkt) {
			t.Fatalf("dispatched n=%d, want %d", g.n, len(ipPkt))
		}
		if !g.src.Equal(src.To4()) || !g.dst.Equal(dst.To4()) {
			t.Fatalf("dispatched src/dst = %s/%s, want %s/%s", g.src, g.dst, src, dst)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("valid packet not dispatched (an invalid packet must be skipped, not stop the loop)")
	}
	// The invalid packet must not have produced a second dispatch.
	select {
	case <-dispatched:
		t.Fatal("unparseable packet should not be dispatched")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestPumpTun_ReadErrorStops verifies a TUN read error stops the loop and is reported on errChan.
func TestPumpTun_ReadErrorStops(t *testing.T) {
	tun := newMockTUN()
	d := &tunDevice{tun: tun, errChan: make(chan error, 1)}
	done := make(chan struct{})
	go func() {
		d.pumpTun(context.Background(), "[Test]", func(buf []byte, _ int, _, _ net.IP) {
			config.LPool.Put(buf[:])
		})
		close(done)
	}()

	tun.Close() // makes Read return net.ErrClosed

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpTun should stop after a TUN read error")
	}
	select {
	case err := <-d.errChan:
		if err == nil {
			t.Fatal("expected a non-nil read error on errChan")
		}
	case <-time.After(time.Second):
		t.Fatal("read error was not reported on errChan")
	}
}

// TestTrySendToSlot_DropsWhenFull verifies a full slot does not block the sender.
func TestTrySendToSlot_DropsWhenFull(t *testing.T) {
	slot := make(chan *Packet, 1)
	first := &Packet{data: config.LPool.Get().([]byte), length: 1}
	trySendToSlot(slot, first) // fills the slot

	dropped := &Packet{data: config.LPool.Get().([]byte), length: 1}
	done := make(chan struct{})
	go func() { trySendToSlot(slot, dropped); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("trySendToSlot must not block when the slot is full")
	}

	if len(slot) != 1 {
		t.Fatalf("expected 1 queued packet, got %d", len(slot))
	}
	if got := <-slot; got != first {
		t.Fatal("the first packet should remain queued; the second should have been dropped")
	}
	config.LPool.Put(first.data[:])
}

// TestBroadcastToSlots_DeliversToAll verifies a heartbeat reaches every slot. All slots
// share one reference-counted buffer (no clone), so each received packet is released
// rather than freed directly — the buffer returns to the pool after the last release.
func TestBroadcastToSlots_DeliversToAll(t *testing.T) {
	const n = 3
	slots := make([]*connSlot, n)
	for i := range slots {
		slots[i] = &connSlot{id: i, inbound: make(chan *Packet, 1)}
	}

	buf := config.LPool.Get().([]byte)
	buf[2] = 1
	payload := buildIPv4Packet(net.IPv4(198, 18, 0, 2), net.IPv4(198, 18, 0, 1), []byte("hb"))
	ln := copy(buf[3:], payload)
	pkt := &Packet{data: buf, length: ln + 1}

	broadcastToSlots(slots, pkt)

	for i := range slots {
		select {
		case got := <-slots[i].inbound:
			if got.length != pkt.length {
				t.Fatalf("slot %d got length %d, want %d", i, got.length, pkt.length)
			}
			got.release()
		default:
			t.Fatalf("slot %d did not receive the broadcast", i)
		}
	}
}
