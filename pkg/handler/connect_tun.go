package handler

import (
	"context"

	"github.com/wencaiwulue/kubevpn/v2/pkg/endpoint"
)

// Run serves the protocol listeners configured by the server command.
func Run(ctx context.Context, servers []endpoint.Server) error {
	return endpoint.Serve(ctx, servers)
}
