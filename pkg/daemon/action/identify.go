package action

import (
	"context"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

func (svr *Server) Identify(ctx context.Context, req *rpc.IdentifyRequest) (*rpc.IdentifyResponse, error) {
	return &rpc.IdentifyResponse{ID: svr.ID}, nil
}
