package action

import (
	"context"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"time"
)

type Server struct {
	rpc.UnimplementedDaemonServer

	timestamp time.Time
}

func (svr *Server) Quit(ctx context.Context, request *rpc.QuitRequest) (*rpc.QuitResponse, error) {
	//TODO implement me
	panic("implement me")
}
