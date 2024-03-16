package util

import (
	"context"
	"io"
	"net"
	"os"
	"time"
)

type timeoutConn struct {
	ctx context.Context
	net.Conn
	readTimeout  time.Duration
	writeTimeout time.Duration
}

func (c *timeoutConn) Write(b []byte) (int, error) {
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	default:
	}

	if c.writeTimeout != 0 {
		err := c.Conn.SetWriteDeadline(time.Now().Add(c.writeTimeout))
		if err != nil {
			return 0, err
		}
	}
	n, err := c.Conn.Write(b)
	if os.IsTimeout(err) {
		return n, io.EOF
	}
	return n, err
}

func (c *timeoutConn) Read(b []byte) (int, error) {
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	default:
	}

	if c.readTimeout != 0 {
		err := c.Conn.SetReadDeadline(time.Now().Add(c.readTimeout))
		if err != nil {
			return 0, err
		}
	}
	n, err := c.Conn.Read(b)
	if os.IsTimeout(err) {
		return n, io.EOF
	}
	return n, err
}

func NewTimeoutWriter(ctx context.Context, w io.Writer, duration time.Duration) io.Writer {
	if duration == 0 {
		return w
	}
	c, ok := w.(net.Conn)
	if !ok {
		return w
	}
	return &timeoutConn{
		ctx:          ctx,
		Conn:         c,
		readTimeout:  duration,
		writeTimeout: duration,
	}
}

func NewTimeoutReader(ctx context.Context, r io.Reader, duration time.Duration) io.Reader {
	if duration == 0 {
		return r
	}
	c, ok := r.(net.Conn)
	if !ok {
		return r
	}
	return &timeoutConn{
		ctx:          ctx,
		Conn:         c,
		readTimeout:  duration,
		writeTimeout: duration,
	}
}

func NewReadWriter(ctx context.Context, rw io.ReadWriter, duration time.Duration) io.ReadWriter {
	if duration == 0 {
		return rw
	}
	c, ok := rw.(net.Conn)
	if !ok {
		return rw
	}
	return &timeoutConn{
		ctx:          ctx,
		Conn:         c,
		readTimeout:  duration,
		writeTimeout: duration,
	}
}

func DisableTimeout(r io.Reader) error {
	if c, ok := r.(*timeoutConn); ok {
		c.readTimeout = 0
		c.writeTimeout = 0
		err := c.Conn.SetReadDeadline(time.Time{})
		err1 := c.Conn.SetWriteDeadline(time.Time{})
		if err != nil {
			return err
		}
		return err1
	}
	return nil
}
