package tunnel

import (
	"context"
	"fmt"
	framing "github.com/wencaiwulue/kubevpn/v2/pkg/internal/wire"
	"net"
)

// Test assembly feeds the same public dial-function boundary used by core.
type testDialer struct {
	Addr        string
	MaxRetries  int
	Transporter interface {
		Dial(context.Context, string) (net.Conn, error)
	}
	Connector interface {
		ConnectContext(context.Context, net.Conn) (net.Conn, error)
	}
}

func (d testDialer) config() *DialConfig {
	return &DialConfig{Addr: d.Addr, DialContext: func(ctx context.Context) (*framing.Conn, error) {
		if d.Addr == "" {
			return nil, fmt.Errorf("empty test dial target")
		}
		var err error
		for range max(1, d.MaxRetries) {
			var c net.Conn
			c, err = d.Transporter.Dial(ctx, d.Addr)
			if err != nil {
				continue
			}
			wrapped, e := d.Connector.ConnectContext(ctx, c)
			if e == nil {
				framed, ok := wrapped.(*framing.Conn)
				if !ok || framed == nil {
					c.Close()
					return nil, fmt.Errorf("test connector returned %T, expected framed connection", wrapped)
				}
				return framed, nil
			}
			c.Close()
			err = e
		}
		return nil, err
	}}
}

type testTCPTransport struct{}

func (testTCPTransport) Dial(ctx context.Context, addr string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
}

type testConnector struct{}

func (testConnector) ConnectContext(ctx context.Context, c net.Conn) (net.Conn, error) {
	return framing.NewConn(ctx, c)
}

type mockTransporter struct {
	dialFn func(context.Context, string) (net.Conn, error)
}

func (m *mockTransporter) Dial(ctx context.Context, addr string) (net.Conn, error) {
	if m.dialFn == nil {
		return nil, fmt.Errorf("no test dial configured")
	}
	return m.dialFn(ctx, addr)
}

type mockConnector struct {
	connectFn func(context.Context, net.Conn) (net.Conn, error)
}

func (m *mockConnector) ConnectContext(ctx context.Context, c net.Conn) (net.Conn, error) {
	return m.connectFn(ctx, c)
}
