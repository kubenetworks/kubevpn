package action

import (
	"github.com/hpcloud/tail"

	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func (svr *Server) Logs(req *rpc.LogRequest, resp rpc.Daemon_LogsServer) error {
	path := GetDaemonLogPath()
	config := tail.Config{Follow: req.Follow, ReOpen: true, MustExist: true}
	if !req.Follow {
		// FATAL -- cannot set ReOpen without Follow.
		config.ReOpen = false
	}
	file, err := tail.TailFile(path, config)
	if err != nil {
		return err
	}
	defer file.Stop()
	for {
		select {
		case <-resp.Context().Done():
			return nil
		case line, ok := <-file.Lines:
			if !ok {
				return nil
			}
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
