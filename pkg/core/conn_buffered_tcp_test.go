package core

import (
	"context"
	"net"
	"testing"
	"time"
)

// Regression for the run() ctx.Done() exit path: when the buffered writer's
// context is cancelled while the writer is idle, run() exits and must mark the
// connection closed so a subsequent Write fails fast instead of silently
// enqueuing into (or blocking on) a channel that nobody will ever drain.
func TestBufferedTCP_WriteFailsFastAfterContextCancel(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	conn := NewBufferedTCP(ctx, serverConn) // run() starts idle, blocked in its select

	// The channel is empty, so the idle run() can only proceed via ctx.Done().
	cancel()
	// Give run() time to observe the cancellation and finish exiting.
	time.Sleep(100 * time.Millisecond)

	// A Write after run() has exited must return an error promptly.
	done := make(chan error, 1)
	go func() {
		_, err := conn.Write([]byte("payload"))
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected Write to fail after context cancellation, got nil error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Write blocked after context cancellation instead of failing fast")
	}
}
