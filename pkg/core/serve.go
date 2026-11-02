package core

import (
	"context"
	"fmt"
	"net"
	"sync"
)

// Serve owns the listeners and accepted handlers until cancellation or a fatal
// device error. It closes their I/O before waiting for all handlers to return.
func Serve(ctx context.Context, servers []Server) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var accepts, handlers sync.WaitGroup
	var mu sync.Mutex
	active := map[net.Conn]struct{}{}
	errors := make(chan error, len(servers))
	fatal := make(chan error, 1)
	// Start service resources before accepting any connection, then join them
	// only after accepted handlers have returned their borrowed resources.
	var services []interface{ Close() error }
	for _, server := range servers {
		if service, ok := server.Handler.(interface {
			Start(context.Context)
			Close() error
		}); ok {
			service.Start(ctx)
			services = append(services, service)
		}
	}
	defer func() {
		for _, service := range services {
			_ = service.Close()
		}
	}()
	for _, server := range servers {
		accepts.Add(1)
		go func(svr Server) {
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
