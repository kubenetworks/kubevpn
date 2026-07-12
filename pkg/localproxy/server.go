package localproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// Connector dials outbound connections on behalf of the proxy server.
type Connector interface {
	Connect(ctx context.Context, host string, port int) (net.Conn, error)
}

// Server runs SOCKS5 and HTTP CONNECT proxy listeners that forward traffic through a Connector.
type Server struct {
	Connector         Connector
	SOCKSListenAddr   string
	HTTPConnectListen string
	Stdout            io.Writer
	Stderr            io.Writer
}

// ListenAndServe starts the configured proxy listeners and blocks until the context is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if s.Connector == nil {
		return fmt.Errorf("connector is required")
	}

	var listeners []net.Listener
	closeAll := func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}
	defer closeAll()

	type serveFn func(context.Context, net.Listener, Connector) error
	type listenerEntry struct {
		name  string
		addr  string
		serve serveFn
	}
	var entries []listenerEntry
	if s.SOCKSListenAddr != "" {
		entries = append(entries, listenerEntry{name: "SOCKS5", addr: s.SOCKSListenAddr, serve: serveSOCKS5})
	}
	if s.HTTPConnectListen != "" {
		entries = append(entries, listenerEntry{name: "HTTP CONNECT", addr: s.HTTPConnectListen, serve: serveHTTPConnect})
	}

	errCh := make(chan error, len(entries))
	var wg sync.WaitGroup
	for _, entry := range entries {
		ln, err := net.Listen("tcp", entry.addr)
		if err != nil {
			return err
		}
		listeners = append(listeners, ln)
		if s.Stdout != nil {
			_, _ = fmt.Fprintf(s.Stdout, "%s proxy listening on %s\n", entry.name, ln.Addr().String())
		}
		wg.Add(1)
		go func(serve serveFn, ln net.Listener) {
			defer wg.Done()
			if err := serve(ctx, ln, s.Connector); err != nil && !isListenerClosedError(err) {
				errCh <- err
			}
		}(entry.serve, ln)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		closeAll()
		<-done
		return nil
	case <-done:
		return nil
	}
}

func isListenerClosedError(err error) bool {
	// Accept()/Read() after Close return an error wrapping net.ErrClosed.
	return errors.Is(err, net.ErrClosed)
}
