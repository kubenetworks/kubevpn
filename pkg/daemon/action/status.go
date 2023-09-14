package action

import (
	"context"

	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func (svr *Server) Status(ctx context.Context, request *rpc.StatusRequest) (*rpc.StatusResponse, error) {
	status := "None"
	if svr.connect != nil {
		status = "Connected"
	}
	return &rpc.StatusResponse{
		Message: status,
	}, nil
}
