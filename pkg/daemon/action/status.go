package action

import (
	"context"

	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func (svr *Server) Status(ctx context.Context, request *rpc.StatusRequest) (*rpc.StatusResponse, error) {
	return &rpc.StatusResponse{
		Message: "i am fine",
	}, nil
}
