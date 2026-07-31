package action

import (
	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

// Unsync handles the Unsync RPC, stopping file synchronization for the specified workloads on the current connection.
func (svr *Server) Unsync(resp rpc.Daemon_UnsyncServer) error {
	req, err := resp.Recv()
	if err != nil {
		return err
	}
	logger, ctx := svr.initStreamLogger(resp, req.Level, func(msg string) error {
		return resp.Send(&rpc.UnsyncResponse{Message: msg})
	})
	svr.connMu.RLock()
	conn, _ := svr.findConnection(svr.currentConnectionID)
	svr.connMu.RUnlock()
	if conn == nil {
		logger.Infof("No connection found")
		return config.ErrConnectionNotFound
	}
	if sync := conn.GetSync(); sync != nil {
		err = sync.Cleanup(ctx, req.Workloads...)
		// SetSync is control-plane-only (ProxyController). Unsync runs in the user
		// daemon where conn is a *ConnectOptions satisfying ProxyController.
		if pc, ok := conn.(handler.ProxyController); ok {
			pc.SetSync(nil)
		}
	}
	return err
}
