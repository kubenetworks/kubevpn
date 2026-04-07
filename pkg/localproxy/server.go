package localproxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
)

type Connector interface {
	Connect(ctx context.Context, host string, port int) (net.Conn, error)
}

type Server struct {
	Connector         Connector
	SOCKSListenAddr   string
	HTTPConnectListen string
	Stdout            io.Writer
	Stderr            io.Writer
}

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
	var entries []struct {
		name  string
		addr  string
		serve serveFn
	}
	if s.SOCKSListenAddr != "" {
		entries = append(entries, struct {
			name  string
			addr  string
			serve serveFn
		}{name: "SOCKS5", addr: s.SOCKSListenAddr, serve: ServeSOCKS5})
	}
	if s.HTTPConnectListen != "" {
		entries = append(entries, struct {
			name  string
			addr  string
			serve serveFn
		}{name: "HTTP CONNECT", addr: s.HTTPConnectListen, serve: ServeHTTPConnect})
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
	if err == nil {
		return false
	}
	if oe, ok := err.(*net.OpError); ok && oe.Err != nil {
		return oe.Err.Error() == "use of closed network connection"
	}
	return err.Error() == "use of closed network connection"
}
