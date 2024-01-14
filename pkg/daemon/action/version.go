package action

import (
	"context"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

func (svr *Server) Version(ctx context.Context, req *rpc.VersionRequest) (*rpc.VersionResponse, error) {
	return &rpc.VersionResponse{Version: config.Version}, nil
}
