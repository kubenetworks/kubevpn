package action

import (
	"github.com/hpcloud/tail"

	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func (svr *Server) Logs(req *rpc.LogRequest, resp rpc.Daemon_LogsServer) error {
	path := GetDaemonLog()
	config := tail.Config{Follow: true, ReOpen: true, MustExist: true}
	file, err := tail.TailFile(path, config)
	if err != nil {
		return err
	}
	for {
		select {
		case <-resp.Context().Done():
			return nil
		case line := <-file.Lines:
			if line.Err != nil {
				return err
			}
			err = resp.Send(&rpc.LogResponse{Message: line.Text})
			if err != nil {
				return err
			}
		}
	}
}
