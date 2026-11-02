package handler

import (
	"context"

	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
)

// Run serves the protocol listeners configured by the server command.
func Run(ctx context.Context, servers []core.Server) error {
	return core.Serve(ctx, servers)
}
