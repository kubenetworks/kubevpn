package action

import (
	"context"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// Identify handles the Identify RPC, returning the daemon's unique instance ID.
func (svr *Server) Identify(ctx context.Context, req *rpc.IdentifyRequest) (*rpc.IdentifyResponse, error) {
	return &rpc.IdentifyResponse{ID: svr.ID}, nil
}
