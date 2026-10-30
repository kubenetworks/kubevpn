package core

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

const maxDatagramPayload = 1<<16 - 1

var errDatagramPayloadSize = errors.New("datagram payload length must be between 0 and 65535")

// NewUDPConnOverTCP wraps a TCP connection with datagram framing for UDP-like semantics.
func NewUDPConnOverTCP(ctx context.Context, conn net.Conn) (net.Conn, error) {
	c := &UDPConnOverTCP{ctx: ctx, Conn: conn}
	// The callback only touches transport state, never the stop function being
	// initialized here (an already-canceled context can start it immediately).
	c.stopCancel = context.AfterFunc(ctx, func() { _ = c.closeTransport() })
	return c, nil
}

var _ net.Conn = (*UDPConnOverTCP)(nil)

// UDPConnOverTCP provides UDP-like datagram semantics over a TCP stream using length-prefix framing.
type UDPConnOverTCP struct {
	net.Conn
	ctx        context.Context
	readMu     sync.Mutex
	writeMu    sync.Mutex
	closed     atomic.Bool
	closeOnce  sync.Once
	closeErr   error
	stopCancel func() bool
}

func (c *UDPConnOverTCP) operationError() error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	if c.closed.Load() {
		return net.ErrClosed
	}
	return nil
}

func (c *UDPConnOverTCP) Read(b []byte) (int, error) {
	if err := c.operationError(); err != nil {
		return 0, err
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if err := c.operationError(); err != nil {
		return 0, err
	}
	datagram, err := readDatagramPacket(c.Conn, b)
	if err != nil {
		// The header may already be consumed. Never reuse a damaged stream.
		_ = c.Close()
		return 0, err
	}
	return int(datagram.DataLength), nil
}

func (c *UDPConnOverTCP) Write(b []byte) (int, error) {
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
		buf = make([]byte, datagramHeaderLen+len(b))
	} else {
		buf = config.LPool.Get().([]byte)
		defer config.LPool.Put(buf)
	}
	copy(buf[datagramHeaderLen:], b)
	binary.BigEndian.PutUint16(buf[:datagramHeaderLen], uint16(len(b)))
	n, err := writeFull(c.Conn, buf[:datagramHeaderLen+len(b)])
	if err != nil {
		_ = c.Close()
	}
	return min(max(n-datagramHeaderLen, 0), len(b)), err
}

// closeTransport never takes an I/O mutex: closing the socket wakes their holders.
func (c *UDPConnOverTCP) closeTransport() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		c.closeErr = c.Conn.Close()
	})
	return c.closeErr
}

func (c *UDPConnOverTCP) Close() error {
	c.stopCancel()
	return c.closeTransport()
}

// DatagramPacket represents a length-prefixed datagram read off a stream connection
// (see readDatagramPacket). Its payload sits at Data[0:DataLength].
type DatagramPacket struct {
	DataLength uint16 // [2]byte
	Data       []byte // []byte
}

// readDatagramPacket uses caller-owned payload storage. On error the caller must
// terminate the stream, since a header or partial body may already be consumed.
func readDatagramPacket(r io.Reader, b []byte) (*DatagramPacket, error) {
	var header [datagramHeaderLen]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	dataLength := binary.BigEndian.Uint16(header[:])
	if int(dataLength) > len(b) {
		return nil, io.ErrShortBuffer
	}
	if _, err := io.ReadFull(r, b[:dataLength]); err != nil {
		return nil, err
	}
	return &DatagramPacket{DataLength: dataLength, Data: b}, nil
}

// writeDatagram stamps the length in the reserved headroom before the payload.
// Stamping is idempotent, but a failed write must terminate this stream; a partly
// transmitted frame must not be replayed on another connection.
func writeDatagram(w io.Writer, buf []byte, payloadLen int) error {
	if payloadLen < 0 || payloadLen > maxDatagramPayload {
		return errDatagramPayloadSize
	}
	if len(buf) < datagramHeaderLen || payloadLen > len(buf)-datagramHeaderLen {
		return io.ErrShortBuffer
	}
	binary.BigEndian.PutUint16(buf[:datagramHeaderLen], uint16(payloadLen))
	_, err := writeFull(w, buf[:datagramHeaderLen+payloadLen])
	return err
}

// writeFull continues positive short writes only on the same writer. The wire
// progress count prevents replaying a partly transmitted frame on another socket.
func writeFull(w io.Writer, data []byte) (int, error) {
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
