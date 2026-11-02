package core

import (
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// framingConn limits individual socket operations to expose fragmentation and
// records transport ownership without replacing the real TCP/TLS connection.
type framingWriteResult struct {
	n   int
	err error
}

type framingConn struct {
	net.Conn
	readLimit, writeLimit     int
	writeError                error
	zero                      bool
	reads, writes, closes     atomic.Int32
	readEntered, writeEntered chan struct{}
	closeError                error
	outcomes                  []framingWriteResult
	beforeClose               func()
}

func (c *framingConn) Read(b []byte) (int, error) {
	c.reads.Add(1)
	if c.readEntered != nil {
		select {
		case c.readEntered <- struct{}{}:
		default:
		}
	}
	if c.readLimit > 0 && len(b) > c.readLimit {
		b = b[:c.readLimit]
	}
	return c.Conn.Read(b)
}
func (c *framingConn) Write(b []byte) (int, error) {
	call := int(c.writes.Add(1)) - 1
	if call < len(c.outcomes) {
		result := c.outcomes[call]
		if result.n == 0 {
			return 0, result.err
		}
		n, err := c.Conn.Write(b[:min(result.n, len(b))])
		if err != nil {
			return n, err
		}
		return n, result.err
	}
	if c.writeEntered != nil {
		select {
		case c.writeEntered <- struct{}{}:
		default:
		}
	}
	if c.zero {
		return 0, nil
	}
	if c.writeLimit > 0 && len(b) > c.writeLimit {
		b = b[:c.writeLimit]
	}
	n, err := c.Conn.Write(b)
	if c.writeError != nil {
		return n, c.writeError
	}
	return n, err
}
func (c *framingConn) Close() error {
	c.closes.Add(1)
	if c.beforeClose != nil {
		c.beforeClose()
	}
	err := c.Conn.Close()
	if c.closeError != nil {
		return c.closeError
	}
	return err
}
func framingWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not finish")
	}
}
func framingNoPanicRead(t *testing.T, c net.Conn, b []byte) (n int, err error) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("framing read panicked: %v", p)
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return c.Read(b)
}
func framingSaturateSocket(t *testing.T, raw, peer net.Conn) {
	t.Helper()
	_ = raw.(*net.TCPConn).SetWriteBuffer(1024)
	_ = peer.(*net.TCPConn).SetReadBuffer(1024)
	// Saturate the actual socket before starting a framed write. The peer never
	// reads, so cancellation must close the transport rather than wait on a gate.
	_ = raw.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	for {
		if _, err := raw.Write(make([]byte, 65535)); err != nil {
			break
		}
	}
	_ = raw.SetWriteDeadline(time.Time{})
}
