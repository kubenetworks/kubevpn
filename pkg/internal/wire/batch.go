package wire

import (
	"encoding/binary"
	"io"
	"net"
)

// BatchWriter belongs to one pump. Frames reserve their length prefixes but do
// not need to be pre-stamped. It borrows the slices (writev may mutate their
// entries) but never retains frame data. The reusable scratch buffer coalesces
// writes through TLS and other wrappers.
type BatchWriter struct{ scratch []byte }

// Write validates and stamps the whole batch before its first socket write,
// then returns wire-byte progress, including progress accompanying an error.
// Only positive short writes on this same connection are continued.
func (w *BatchWriter) Write(conn net.Conn, frames [][]byte) (int, error) {
	total := 0
	for _, frame := range frames {
		if len(frame) < HeaderLen {
			return 0, io.ErrShortBuffer
		}
		if len(frame)-HeaderLen > maxDatagramPayload {
			return 0, errDatagramPayloadSize
		}
		total += len(frame)
	}
	for _, frame := range frames {
		binary.BigEndian.PutUint16(frame[:HeaderLen], uint16(len(frame)-HeaderLen))
	}
	written := 0
	if tcp, ok := conn.(*net.TCPConn); ok {
		bufs := net.Buffers(frames)
		for written < total {
			n, err := bufs.WriteTo(tcp)
			written += int(n)
			if err != nil {
				return written, err
			}
			if n == 0 {
				return written, io.ErrShortWrite
			}
		}
		return written, nil
	}
	var data []byte
	if len(frames) == 1 {
		data = frames[0]
	} else {
		if cap(w.scratch) < total {
			w.scratch = make([]byte, total)
		}
		data = w.scratch[:total]
		off := 0
		for _, frame := range frames {
			off += copy(data[off:], frame)
		}
	}
	return WriteFull(conn, data)
}
