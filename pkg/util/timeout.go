package util

import (
	"io"
	"net"
	"time"
)

type timeoutConn struct {
	conn         net.Conn
	readTimeout  time.Duration
	writeTimeout time.Duration
}

func (c *timeoutConn) Write(b []byte) (int, error) {
	if c.writeTimeout != 0 {
		err := c.conn.SetWriteDeadline(time.Now().Add(c.writeTimeout))
		if err != nil {
			return 0, err
		}
	}
	return c.conn.Write(b)
}

func (c *timeoutConn) Read(b []byte) (int, error) {
	if c.readTimeout != 0 {
		err := c.conn.SetReadDeadline(time.Now().Add(c.readTimeout))
		if err != nil {
			return 0, err
		}
	}
	return c.conn.Read(b)
}

func NewTimeoutWriter(w io.Writer, duration time.Duration) io.Writer {
	if duration == 0 {
		return w
	}
	c, ok := w.(net.Conn)
	if !ok {
		return w
	}
	return &timeoutConn{
		conn:         c,
		readTimeout:  duration,
		writeTimeout: duration,
	}
}

func NewTimeoutReader(r io.Reader, duration time.Duration) io.Reader {
	if duration == 0 {
		return r
	}
	c, ok := r.(net.Conn)
	if !ok {
		return r
	}
	return &timeoutConn{
		conn:         c,
		readTimeout:  duration,
		writeTimeout: duration,
	}
}

func NewReadWriter(rw io.ReadWriter, duration time.Duration) io.ReadWriter {
	if duration == 0 {
		return rw
	}
	c, ok := rw.(net.Conn)
	if !ok {
		return rw
	}
	return &timeoutConn{
		conn:         c,
		readTimeout:  duration,
		writeTimeout: duration,
	}
}

func DisableTimeout(r io.Reader) error {
	if c, ok := r.(*timeoutConn); ok {
		c.readTimeout = 0
		c.writeTimeout = 0
		err := c.conn.SetReadDeadline(time.Time{})
		err1 := c.conn.SetWriteDeadline(time.Time{})
		if err != nil {
			return err
		}
		return err1
	}
	return nil
}
