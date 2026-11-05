package tunnel

import (
	"context"
	"net"

	"github.com/wencaiwulue/kubevpn/v2/pkg/internal/wire"
)

const (
	maxBatchFrames = 32
	maxBatchBytes  = 64 * 1024
)

// packetBatch belongs to one consumer, never to a peer-wide or global writer.
// pending retains the first frame beyond the byte budget without requeueing it.
// Only dequeued packets belong here; a reconnect leaves the rest of the queue intact.
type packetBatch struct {
	packets [maxBatchFrames]*packet
	n       int
	pending *packet
}

func (b *packetBatch) next(ctx context.Context, ch <-chan *packet, limit int, stop <-chan struct{}) bool {
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
	writer  wire.BatchWriter
}

// write returns the number of fully written frames, including a full prefix
// preceding an error. A partial frame is continued only on this same connection.
func (w *frameBatchWriter) write(conn net.Conn, packets []*packet) (int, error) {
	for i, p := range packets {
		w.buffers[i] = p.data[:datagramHeaderLen+p.length]
	}
	defer clear(w.buffers[:])
	written, err := w.writer.Write(conn, w.buffers[:len(packets)])
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
