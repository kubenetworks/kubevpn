package core

import (
	"context"
	"io"
	"net"
)

const (
	maxBatchFrames = 32
	maxBatchBytes  = 64 * 1024
)

// packetBatch belongs to one consumer, never to a peer-wide or global writer.
// pending retains the first frame beyond the byte budget without requeueing it.
// Only dequeued packets belong here; a reconnect leaves the rest of the queue intact.
type packetBatch struct {
	packets [maxBatchFrames]*Packet
	n       int
	pending *Packet
}

func (b *packetBatch) next(ctx context.Context, ch <-chan *Packet, limit int, stop <-chan struct{}) bool {
	if ctx.Err() != nil {
		return false
	}
	first := b.pending
	b.pending = nil
	if first == nil {
		select {
		case first = <-ch:
		case <-ctx.Done():
			return false
		case <-stop:
			return false
		}
	}
	if first == nil {
		return false
	}
	b.packets[0] = first
	b.n = 1
	size := datagramHeaderLen + first.length
	for b.n < limit && size < maxBatchBytes {
		select {
		case p := <-ch:
			if p == nil {
				return true
			}
			n := datagramHeaderLen + p.length
			if size+n > maxBatchBytes {
				b.pending = p
				return true
			}
			b.packets[b.n] = p
			b.n++
			size += n
		default:
			return true
		}
	}
	return true
}

func (b *packetBatch) release() {
	for i := 0; i < b.n; i++ {
		b.packets[i].release()
		b.packets[i] = nil
	}
	b.n = 0
}
func (b *packetBatch) close() {
	b.release()
	if b.pending != nil {
		b.pending.release()
		b.pending = nil
	}
}

// frameBatchWriter reuses one bounded aggregation buffer for TLS and wrappers.
// net.Buffers only uses writev for a concrete TCPConn; on TLS it would issue a
// separate TLS Write for each frame, defeating batching.
type frameBatchWriter struct {
	buffers [maxBatchFrames][]byte
	scratch []byte
}

// write returns the number of fully written frames, including a full prefix
// preceding an error. A partial frame is continued only on this same connection.
func (w *frameBatchWriter) write(conn net.Conn, packets []*Packet) (int, error) {
	total := 0
	for i, p := range packets {
		w.buffers[i] = p.data[:datagramHeaderLen+p.length]
		total += len(w.buffers[i])
	}
	defer func() { clear(w.buffers[:]) }()
	var written int
	var err error
	if tcp, ok := conn.(*net.TCPConn); ok {
		bufs := net.Buffers(w.buffers[:len(packets)])
		for written < total {
			n, e := bufs.WriteTo(tcp)
			written += int(n)
			if e != nil {
				err = e
				break
			}
			if n == 0 {
				err = io.ErrShortWrite
				break
			}
		}
	} else {
		var data []byte
		if len(packets) == 1 {
			data = w.buffers[0]
		} else {
			if cap(w.scratch) < total {
				w.scratch = make([]byte, total)
			}
			data = w.scratch[:total]
			off := 0
			for _, buf := range w.buffers[:len(packets)] {
				off += copy(data[off:], buf)
			}
		}
		for written < total {
			n, e := conn.Write(data[written:])
			written += n
			if e != nil {
				err = e
				break
			}
			if n == 0 {
				err = io.ErrShortWrite
				break
			}
		}
	}
	complete := 0
	for _, p := range packets {
		n := datagramHeaderLen + p.length
		if written < n {
			break
		}
		written -= n
		complete++
	}
	return complete, err
}
