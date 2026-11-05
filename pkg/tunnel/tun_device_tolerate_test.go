package tunnel

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// recoverTUN returns an error on the first N reads, then delivers one packet, then parks.
// It models the macOS wireguard route-listener surfacing a transient error once while the
// underlying fd stays usable.
type recoverTUN struct {
	*mockTUN
	mu        sync.Mutex
	errsLeft  int
	err       error
	packet    []byte
	delivered bool
	park      chan struct{}
}

func (c *recoverTUN) Read(b []byte) (int, error) {
	c.mu.Lock()
	if c.errsLeft > 0 {
		c.errsLeft--
		c.mu.Unlock()
		return 0, c.err
	}
	if !c.delivered {
		c.delivered = true
		n := copy(b, c.packet)
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()
	<-c.park // idle: block like a real TUN with no traffic
	return 0, net.ErrClosed
}

// TestPumpTun_ToleratesTransientReadError verifies a one-off read error does not tear down the
// device: the loop retries and delivers the next packet, and nothing is written to errChan.
func TestPumpTun_ToleratesTransientReadError(t *testing.T) {
	tun := &recoverTUN{
		mockTUN:  newMockTUN(),
		errsLeft: 1,
		err:      errors.New("route ip+net: no buffer space available"),
		packet:   buildIPv4Packet(net.IPv4(198, 18, 0, 2), net.IPv4(10, 0, 0, 5), []byte("hello")),
		park:     make(chan struct{}),
	}
	d := &tunDevice{tun: tun, errChan: make(chan error, 1)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dispatched := make(chan struct{}, 1)
	go d.pumpTun(ctx, "[Test]", func(buf []byte, _ int, _, _ net.IP) {
		config.LPool.Put(buf[:])
		select {
		case dispatched <- struct{}{}:
		default:
		}
	})

	select {
	case <-dispatched:
		// good: survived the transient error and delivered the following packet
	case <-time.After(3 * time.Second):
		t.Fatal("packet after a transient read error was not dispatched")
	}
	select {
	case err := <-d.errChan:
		t.Fatalf("a one-off read error must not be reported as fatal: %v", err)
	default:
	}

	cancel()
	close(tun.park)
}

func TestStage4PumpErrorThresholdAndReset(t *testing.T) {
	for _, cancelOnRead := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		pool := &stage4PumpPool{live: make(map[*byte]bool)}
		d := &tunDevice{errChan: make(chan error, 1)}
		reads, delivered := 0, 0
		ip := buildIPv4Packet(net.IPv4(198, 18, 0, 2), net.IPv4(10, 0, 0, 1), nil)
		f := &stage4PumpSource{batch: 128}
		f.read = func(b [][]byte, s []int, off int) (int, error) {
			reads++
			copy(b[0][off:], ip)
			s[0] = len(ip)
			if cancelOnRead {
				cancel()
			}
			if reads == 3 {
				return 1, nil
			} // resets the preceding two errors
			return 1, net.ErrClosed
		}
		d.pumpTunBatchWithPool(ctx, "stage4", f, func(b []byte, _ int, _, _ net.IP) { delivered++; pool.Put(b) }, pool)
		if cancelOnRead {
			if reads != 1 || delivered != 0 {
				t.Fatal("canceled read dispatched prefix")
			}
			select {
			case e := <-d.errChan:
				t.Fatalf("cancel fatal: %v", e)
			default:
			}
		} else {
			if reads != 13 || delivered != 13 {
				t.Fatalf("read/error reset counts=%d/%d", reads, delivered)
			}
			select {
			case e := <-d.errChan:
				if e != net.ErrClosed {
					t.Fatal(e)
				}
			default:
				t.Fatal("missing fatal tenth error")
			}
		}
		if pool.gets != pool.puts || len(pool.live) != 0 {
			t.Fatalf("cleanup get/put %d/%d", pool.gets, pool.puts)
		}
	}
}

// TestPumpTun_CtxCancelQuietExit verifies that when the context is cancelled, a resulting read
// error is treated as a clean shutdown — the loop returns without reporting an error.
func TestPumpTun_CtxCancelQuietExit(t *testing.T) {
	tun := newMockTUN()
	d := &tunDevice{tun: tun, errChan: make(chan error, 1)}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.pumpTun(ctx, "[Test]", func(buf []byte, _ int, _, _ net.IP) { config.LPool.Put(buf[:]) })
		close(done)
	}()

	// Cancel first, then unblock Read with a close so it returns an error while ctx is done.
	cancel()
	tun.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpTun should return promptly after context cancellation")
	}
	select {
	case err := <-d.errChan:
		t.Fatalf("cancellation should not report an error on errChan: %v", err)
	default:
	}
}
