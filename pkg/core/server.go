package core

import (
	"context"
	"net"

	log "github.com/sirupsen/logrus"
)

type Server struct {
	Listener net.Listener
	Handler  Handler
}

// Serve serves as a proxy server.
func (s *Server) Serve(ctx context.Context) error {
	l := s.Listener
	defer l.Close()
	//go func() {
	//	<-ctx.Done()
	//	l.Close()
	//}()
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			conn, err := l.Accept()
			if err != nil {
				log.Warnf("server: accept error: %v", err)
				continue
			}
			go s.Handler.Handle(ctx, conn)
		}

	}
}
