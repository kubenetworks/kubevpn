package core

import (
	"context"
	"net"
	"time"

	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/util/retry"
)

type Server struct {
	Listener net.Listener
	Handler  Handler
}

// Serve serves as a proxy server.
func (s *Server) Serve(ctx context.Context) error {
	l := s.Listener
	defer l.Close()
	var tempDelay time.Duration
	go func() {
		<-ctx.Done()
		if err := retry.OnError(retry.DefaultBackoff, func(err error) bool { return err != nil }, l.Close); err != nil {
			log.Warnf("error while close listener, err: %v", err)
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

		go s.Handler.Handle(ctx, conn)
	}
	return nil
}
