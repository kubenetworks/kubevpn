package core

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// deadlineRecordingConn records the last read deadline set on it, so a test can assert that
// write activity pushes the read (liveness) deadline forward.
type deadlineRecordingConn struct {
	net.Conn
	mu   sync.Mutex
	read time.Time
}

func (c *deadlineRecordingConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.read = t
	c.mu.Unlock()
	return c.Conn.SetReadDeadline(t)
}

func (c *deadlineRecordingConn) lastReadDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.read
}

// TestWriteToConn_RefreshesReadDeadline verifies the Fix A liveness rule: a slot that is
// actively writing pushes its read (liveness) deadline forward, so an actively-sending slot is
// not torn down by readFromConn's read-idle timeout even when it receives no reverse traffic.
func TestWriteToConn_RefreshesReadDeadline(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	// Drain the server side so the pipe write completes.
	go io.Copy(io.Discard, server)

	rec := &deadlineRecordingConn{Conn: client}
	slot := &connSlot{id: 1, inbound: make(chan *Packet, 4)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errChan := make(chan error, 2)
	go slot.writeToConn(ctx, rec, errChan)

	// Enqueue one packet for the slot to write.
	buf := config.LPool.Get().([]byte)[:]
	slot.inbound <- NewPacket(buf, 10, nil, nil)

	// The read deadline should be pushed to ~ now + KeepAliveTime*3 shortly after the write.
	before := time.Now()
	var got time.Time
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if got = rec.lastReadDeadline(); !got.IsZero() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got.IsZero() {
		t.Fatal("writeToConn did not refresh the read deadline after a successful write")
	}
	want := before.Add(config.KeepAliveTime * 3)
	if diff := got.Sub(want); diff > 5*time.Second || diff < -5*time.Second {
		t.Errorf("read deadline = %v (%.0fs from now), want ~%v (KeepAliveTime*3 ahead)", got, time.Until(got).Seconds(), want)
	}

	select {
	case err := <-errChan:
		t.Fatalf("writeToConn exited unexpectedly: %v", err)
	default:
	}
}
