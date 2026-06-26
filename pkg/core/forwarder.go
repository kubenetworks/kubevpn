package core

import (
	"context"
	"errors"
	"net"
)

var ErrorEmptyForwarder = errors.New("empty forwarder")

// ParseForwarder creates a Forwarder from a remote URI string (e.g. "tcp://host:port").
func ParseForwarder(remote string) (*Forwarder, error) {
	node, err := ParseNode(remote)
	if err != nil {
		return nil, err
	}
	return &Forwarder{
		Addr:        node.Addr,
		Connector:   NewUDPOverTCPConnector(),
		Transporter: TCPTransporter(nil),
		MaxRetries:  5,
	}, nil
}

// Forwarder dials a remote address with retry, applying transport and connection wrapping.
type Forwarder struct {
	Addr        string
	Connector   Connector
	Transporter Transporter
	MaxRetries  int
}

func (f *Forwarder) IsEmpty() bool {
	return f == nil || f.Addr == ""
}

func (f *Forwarder) DialContext(ctx context.Context) (conn net.Conn, err error) {
	for i := 0; i < max(1, f.MaxRetries); i++ {
		conn, err = f.dial(ctx)
		if err == nil {
			break
		}
	}
	return
}

func (f *Forwarder) dial(ctx context.Context) (net.Conn, error) {
	if f.IsEmpty() {
		return nil, ErrorEmptyForwarder
	}
	conn, err := f.Transporter.Dial(ctx, f.Addr)
	if err != nil {
		return nil, err
	}
	cc, err := f.Connector.ConnectContext(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return cc, nil
}

// Handler handles an inbound connection.
type Handler interface {
	Handle(ctx context.Context, conn net.Conn)
}

// Connector wraps a raw connection (e.g. TCP keep-alive, UDP-over-TCP).
type Connector interface {
	ConnectContext(ctx context.Context, conn net.Conn) (net.Conn, error)
}

// Transporter establishes a transport-level connection to an address.
type Transporter interface {
	Dial(ctx context.Context, addr string) (net.Conn, error)
}

// Server pairs a listener with a handler.
type Server struct {
	Listener net.Listener
	Handler  Handler
}
