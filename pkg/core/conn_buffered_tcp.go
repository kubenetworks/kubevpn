package core

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

type bufferedTCP struct {
	net.Conn
	ch       chan *Packet
	closed   atomic.Bool
	done     chan struct{} // stops enqueues; run joins producers before its final drain
	enqueue  sync.RWMutex
	stopOnce sync.Once
}

// NewBufferedTCP wraps a TCP connection with an async write buffer to reduce syscalls.
func NewBufferedTCP(ctx context.Context, conn net.Conn) net.Conn {
	c := &bufferedTCP{Conn: conn, ch: make(chan *Packet, MaxSize), done: make(chan struct{})}
	go c.run(ctx)
	return c
}

// writePacket transfers one reference only on success. The read lock joins an
// enqueue already in flight with run's final drain, including a select choosing
// the queue just as done closes. The channel itself is never closed.
func (c *bufferedTCP) writePacket(pkt *Packet) bool {
	c.enqueue.RLock()
	defer c.enqueue.RUnlock()
	if c.closed.Load() {
		return false
	}
	select {
	case c.ch <- pkt:
		return true
	case <-c.done:
		return false
	}
}

// Write satisfies net.Conn. b is one fully framed datagram and may be reused
// by the caller immediately; routing paths use writePacket to avoid this copy.
func (c *bufferedTCP) Write(b []byte) (int, error) {
	if c.closed.Load() {
		return 0, errors.New("tcp channel is closed")
	}
	if len(b) < datagramHeaderLen {
		return 0, nil
	}
	if len(b) > 65535+datagramHeaderLen {
		return 0, errors.New("tcp frame too large")
	}
	buf := config.LPool.Get().([]byte)
	if len(b) > len(buf) {
		config.LPool.Put(buf)
		buf = make([]byte, len(b))
	}
	n := copy(buf, b)
	pkt := NewPacket(buf, n-datagramHeaderLen, nil, nil)
	if !c.writePacket(pkt) {
		pkt.release()
		return 0, errors.New("tcp channel is closed")
	}
	return n, nil
}

func (c *bufferedTCP) stop() {
	c.stopOnce.Do(func() { c.closed.Store(true); close(c.done) })
}

func (c *bufferedTCP) run(ctx context.Context) {
	stopClose := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stopClose()
	var batch packetBatch
	defer func() {
		c.stop()
		batch.close()
		// done unblocks even a producer holding RLock on a full queue. Once
		// joined, no successful enqueue can escape this final drain.
		c.enqueue.Lock()
		drainPacketChan(c.ch)
		c.enqueue.Unlock()
	}()
	var writer frameBatchWriter
	dl := &periodicDeadline{timeout: config.KeepAliveTime, set: c.Conn.SetWriteDeadline}
	for !c.closed.Load() && batch.next(ctx, c.ch, maxBatchFrames, c.done) {
		if err := dl.refresh(); err != nil {
			plog.G(ctx).Errorf("[TCP] Failed to set write deadline: %v", err)
			_ = c.Close()
			return
		}
		_, err := writer.write(c.Conn, batch.packets[:batch.n])
		batch.release()
		if err != nil {
			plog.G(ctx).Errorf("[TCP] Failed to write packet: %v", err)
			_ = c.Close()
			return
		}
	}
}

func (c *bufferedTCP) Close() error {
	c.stop()
	return c.Conn.Close()
}
