package core

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/wencaiwulue/kubevpn/v2/pkg/internal/wire"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tunnel"
)

var errEmptyForwarder = errors.New("empty forwarder")

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

// IsEmpty reports whether the forwarder has no target address configured.
func (f *Forwarder) IsEmpty() bool {
	return f == nil || f.Addr == ""
}

// DialContext dials the remote address with retries up to MaxRetries.
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
		return nil, errEmptyForwarder
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

// TunnelConfig passes only dialing behavior across the data-plane boundary.
func (f *Forwarder) TunnelConfig() *tunnel.DialConfig {
	if f.IsEmpty() {
		return nil
	}
	return &tunnel.DialConfig{Addr: f.Addr, DialContext: func(ctx context.Context) (*wire.Conn, error) {
		conn, err := f.DialContext(ctx)
		if err != nil {
			return nil, err
		}
		framed, ok := conn.(*wire.Conn)
		if !ok || framed == nil {
			if !ok && conn != nil {
				_ = conn.Close()
			}
			return nil, fmt.Errorf("tunnel connector returned %T, expected non-nil *wire.Conn", conn)
		}
		return framed, nil
	}}
}
