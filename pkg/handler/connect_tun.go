package handler

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
)

// A TUN handler exiting is terminal even if its listener's Accept is still
// blocked. Cancel the manager; its external owner performs joined cleanup.
type tunCompletionHandler struct {
	core.Handler
	cancel context.CancelFunc
}

func (h *tunCompletionHandler) Handle(ctx context.Context, c net.Conn) {
	defer h.cancel()
	h.Handler.Handle(ctx, c)
}

// Run owns both the accept loops and accepted handlers. Cancellation closes the
// listeners and connections, then waits for actual handler completion.
func Run(ctx context.Context, servers []core.Server) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var accepts, handlers sync.WaitGroup
	var mu sync.Mutex
	active := map[net.Conn]struct{}{}
	errors := make(chan error, len(servers))
	fatal := make(chan error, 1)
	for _, server := range servers {
		accepts.Add(1)
		go func(svr core.Server) {
			defer accepts.Done()
			for {
				c, err := svr.Listener.Accept()
				if err != nil {
					errors <- err
					return
				}
				mu.Lock()
				if ctx.Err() != nil {
					mu.Unlock()
					c.Close()
					return
				}
				active[c] = struct{}{}
				handlers.Add(1)
				mu.Unlock()
				go func() {
					defer handlers.Done()
					defer func() { c.Close(); mu.Lock(); delete(active, c); mu.Unlock() }()
					svr.Handler.Handle(ctx, c)
					if h, ok := svr.Handler.(interface{ FatalOnReturn() bool }); ok && h.FatalOnReturn() && ctx.Err() == nil {
						select {
						case fatal <- fmt.Errorf("server device handler exited"):
						default:
						}
					}
				}()
			}
		}(server)
	}
	var err error
	select {
	case err = <-errors:
	case err = <-fatal:
	case <-ctx.Done():
		err = ctx.Err()
	}
	cancel()
	for _, svr := range servers {
		svr.Listener.Close()
	}
	accepts.Wait()
	mu.Lock()
	conns := make([]net.Conn, 0, len(active))
	for c := range active {
		conns = append(conns, c)
	}
	mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
	handlers.Wait()
	return err
}
