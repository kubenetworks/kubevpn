package action

import (
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func (svr *Server) Remove(req *rpc.RemoveRequest, resp rpc.Daemon_RemoveServer) error {
	return nil
}
