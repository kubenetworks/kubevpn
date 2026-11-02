package tunnel

import (
	"context"
	"sync"
)

// tunBridge owns device admission and drain independently of the route table.
type tunBridge struct {
	queue   chan *packet
	done    chan struct{}
	once    sync.Once
	enqueue sync.RWMutex
}

func newTUNBridge() *tunBridge {
	return &tunBridge{queue: make(chan *packet, MaxSize), done: make(chan struct{})}
}
func (b *tunBridge) send(ctx context.Context, pkt *packet) bool {
	b.enqueue.RLock()
	defer b.enqueue.RUnlock()
	if ctx.Err() != nil {
		return false
	}
	select {
	case <-b.done:
		return false
	default:
	}
	select {
	case b.queue <- pkt:
		return true
	case <-b.done:
		return false
	case <-ctx.Done():
		return false
	}
}
func (b *tunBridge) stop() {
	b.once.Do(func() { close(b.done); b.enqueue.Lock(); drainPacketChan(b.queue); b.enqueue.Unlock() })
}
