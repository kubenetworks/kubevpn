package core

import (
	"context"
	"testing"
	"time"
)

// TestServe_CleanShutdownOnCancel verifies the WireGuard-style lifecycle: serve starts every
// device routine, and when the context is cancelled it cancels + closes the TUN and drains all
// routines, returning within a timeout (no leaked/hung goroutines).
func TestServe_CleanShutdownOnCancel(t *testing.T) {
	run := func(t *testing.T, makeTransport func(*tunDevice) transport) {
		tun := newMockTUN()
		dev := &tunDevice{
			tun:         tun,
			tunInbound:  make(chan *Packet, MaxSize),
			tunOutbound: make(chan *Packet, MaxSize),
			errChan:     make(chan error, 1),
		}
		dev.transport = makeTransport(dev)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			serve(ctx, dev)
			close(done)
		}()

		// Let the routines start, then cancel and require a prompt, clean return.
		time.Sleep(100 * time.Millisecond)
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("serve did not return after ctx cancel (routines not drained)")
		}
	}

	t.Run("server", func(t *testing.T) {
		run(t, func(dev *tunDevice) transport { return newServerTransport(dev, NewRouteHub()) })
	})
	t.Run("client", func(t *testing.T) {
		// forward is empty, so dial attempts fail fast and the slots just loop+backoff until
		// cancel — exercising the conn-pool/heartbeat routines' shutdown path.
		run(t, func(dev *tunDevice) transport { return newClientTransport(dev, &Forwarder{}) })
	})
}
