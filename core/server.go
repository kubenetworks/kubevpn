package core

import (
	"context"
	"k8s.io/client-go/util/retry"
	"net"
	"time"

	log "github.com/sirupsen/logrus"
)

// Server is a proxy server.
type Server struct {
	Listener net.Listener
	Handler  Handler
}

// Addr returns the address of the server
func (s *Server) Addr() net.Addr {
	return s.Listener.Addr()
}

// Close closes the server
func (s *Server) Close() error {
	return s.Listener.Close()
}

// Serve serves as a proxy server.
func (s *Server) Serve(ctx context.Context, h Handler) error {
	if h == nil {
		h = s.Handler
	}

	l := s.Listener
	var tempDelay time.Duration
	go func() {
		select {
		case <-ctx.Done():
			err := retry.OnError(retry.DefaultBackoff, func(err error) bool {
				return err != nil
			}, func() error {
				return l.Close()
			})
			if err != nil {
				log.Warnf("error while close listener, err: %v", err)
			}
		}
	}()
	for ctx.Err() == nil {
		conn, e := l.Accept()
		if e != nil {
			if ne, ok := e.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				log.Warnf("server: Accept error: %v; retrying in %v", e, tempDelay)
				time.Sleep(tempDelay)
				continue
			}
			return e
		}
		tempDelay = 0

		go h.Handle(ctx, conn)
	}
	return nil
}
