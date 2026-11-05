package wire

import (
	"io"
	"net"
)

// BatchWriter belongs to one pump. Frames include their length prefixes. It
// borrows the slice (writev may mutate its entries) but never retains frame data.
// The reusable scratch buffer coalesces writes through TLS and other wrappers.
type BatchWriter struct{ scratch []byte }

// Write returns wire-byte progress, including progress accompanying an error.
// Only positive short writes on this same connection are continued.
func (w *BatchWriter) Write(conn net.Conn, frames [][]byte) (int, error) {
	total := 0
	for _, frame := range frames {
		total += len(frame)
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
