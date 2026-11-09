package wire

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

const HeaderLen = 2

const maxDatagramPayload = 1<<16 - 1

var errDatagramPayloadSize = errors.New("datagram payload length must be between 0 and 65535")

// NewConn wraps a TCP connection with datagram framing for UDP-like semantics.
func NewConn(ctx context.Context, conn net.Conn) (net.Conn, error) {
	c := &Conn{ctx: ctx, Conn: conn, cancelDone: make(chan struct{})}
	// The callback only touches transport state, never the stop function being
	// initialized here (an already-canceled context can start it immediately).
	c.stopCancel = context.AfterFunc(ctx, func() { defer close(c.cancelDone); _ = c.closeTransport() })
	return c, nil
}

var _ net.Conn = (*Conn)(nil)

// Conn provides UDP-like datagram semantics over a TCP stream using length-prefix framing.
type Conn struct {
	net.Conn
	ctx        context.Context
	readMu     sync.Mutex
	writeMu    sync.Mutex
	closed     atomic.Bool
	closeOnce  sync.Once
	closeErr   error
	stopCancel func() bool
	cancelDone chan struct{}
	fault      func()
}

func (c *Conn) operationError() error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	if c.closed.Load() {
		return net.ErrClosed
	}
	return nil
}

func (c *Conn) Read(b []byte) (int, error) {
	if err := c.operationError(); err != nil {
		return 0, err
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if err := c.operationError(); err != nil {
		return 0, err
	}
	n, err := ReadDatagram(c.Conn, b)
	if err != nil {
		// The header may already be consumed. Never reuse a damaged stream.
		if c.fault != nil {
			c.fault()
		}
		_ = c.Close()
		return 0, err
	}
	return n, nil
}

func (c *Conn) Write(b []byte) (int, error) {
	if len(b) > maxDatagramPayload {
		return 0, errDatagramPayloadSize
	}
	if err := c.operationError(); err != nil {
		return 0, err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.operationError(); err != nil {
		return 0, err
	}

	var buf []byte
	if len(b) == maxDatagramPayload {
		// The largest legal frame exceeds LPool's 64KiB buffers by one byte.
		buf = make([]byte, HeaderLen+len(b))
	} else {
		buf = config.LPool.Get().([]byte)
		defer config.LPool.Put(buf)
	}
	copy(buf[HeaderLen:], b)
	binary.BigEndian.PutUint16(buf[:HeaderLen], uint16(len(b)))
	n, err := WriteFull(c.Conn, buf[:HeaderLen+len(b)])
	if err != nil {
		if c.fault != nil {
			c.fault()
		}
		_ = c.Close()
	}
	return min(max(n-HeaderLen, 0), len(b)), err
}

// closeTransport never takes an I/O mutex: closing the socket wakes their holders.
func (c *Conn) closeTransport() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		c.closeErr = c.Conn.Close()
	})
	return c.closeErr
}

func (c *Conn) Close() error {
	if c.stopCancel != nil {
		c.stopCancel()
	}
	return c.closeTransport()
}

// ReadDatagram uses caller-owned payload storage and returns the payload length.
// On error the caller must terminate the stream, since a header or partial body
// may already be consumed.
func ReadDatagram(r io.Reader, b []byte) (int, error) {
	var header [HeaderLen]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, err
	}
	dataLength := int(binary.BigEndian.Uint16(header[:]))
	if dataLength > len(b) {
		return 0, io.ErrShortBuffer
	}
	if _, err := io.ReadFull(r, b[:dataLength]); err != nil {
		return 0, err
	}
	return dataLength, nil
}

// WriteDatagram stamps the length in the reserved headroom before the payload.
// Stamping is idempotent, but a failed write must terminate this stream; a partly
// transmitted frame must not be replayed on another connection.
func WriteDatagram(w io.Writer, buf []byte, payloadLen int) error {
	if payloadLen < 0 || payloadLen > maxDatagramPayload {
		return errDatagramPayloadSize
	}
	if len(buf) < HeaderLen || payloadLen > len(buf)-HeaderLen {
		return io.ErrShortBuffer
	}
	binary.BigEndian.PutUint16(buf[:HeaderLen], uint16(payloadLen))
	_, err := WriteFull(w, buf[:HeaderLen+payloadLen])
	return err
}

// WriteFull continues positive short writes only on the same writer. The wire
// progress count prevents replaying a partly transmitted frame on another socket.
func WriteFull(w io.Writer, data []byte) (int, error) {
	written := 0
	for written < len(data) {
		n, err := w.Write(data[written:])
		written += n
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

// TakeOwnership stops and joins the constructor cancellation callback. The caller
// now owns cancellation and Close. Call before starting any Read/Write operations.
func (c *Conn) TakeOwnership() {
	if c.stopCancel != nil && !c.stopCancel() && c.cancelDone != nil {
		<-c.cancelDone
	}
	c.stopCancel = nil
}

// NewView creates framing for an externally owned connection, without installing
// a cancellation callback. Framing faults invoke beforeClose before transport Close.
func NewView(ctx context.Context, conn net.Conn, beforeClose func()) *Conn {
	return &Conn{ctx: ctx, Conn: conn, fault: beforeClose}
}
