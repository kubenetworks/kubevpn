package tunnel

import (
	"context"
	"net"

	"github.com/wencaiwulue/kubevpn/v2/pkg/internal/wire"
)

// DialConfig contains assembly-provided connection establishment. Retry, TLS and
// connector policy stay with the caller; tunnel only schedules slot attempts.
// A successful dial returns a non-nil framed connection, never a raw stream.
type DialConfig struct {
	Addr        string
	DialContext func(context.Context) (*wire.Conn, error)
}

func (d *DialConfig) IsEmpty() bool { return d == nil || d.DialContext == nil }

// Handler consumes a connection; factories in core provide the listener.
type Handler interface {
	Handle(context.Context, net.Conn)
}
