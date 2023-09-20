package action

import (
	"fmt"
	"io"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func (svr *Server) Disconnect(req *rpc.DisconnectRequest, resp rpc.Daemon_DisconnectServer) error {
	if !svr.IsSudo {
		cli := svr.GetClient(true)
		if cli == nil {
			return fmt.Errorf("sudo daemon not start")
		}
		connResp, err := cli.Disconnect(resp.Context(), req)
		if err != nil {
			return err
		}
		var recv *rpc.DisconnectResponse
		for {
			recv, err = connResp.Recv()
			if err == io.EOF {
				svr.t = time.Time{}
				svr.connect = nil
				return nil
			} else if err != nil {
				return err
			}
			err = resp.Send(recv)
			if err != nil {
				return err
			}
		}
	}

	defer func() {
		log.SetOutput(svr.LogFile)
		log.SetLevel(log.DebugLevel)
	}()
	out := io.MultiWriter(newDisconnectWarp(resp), svr.LogFile)
	log.SetOutput(out)
	log.SetLevel(log.InfoLevel)

	if svr.connect != nil {
		svr.connect.Cleanup()
	}
	svr.t = time.Time{}
	svr.connect = nil
	return nil
}

type disconnectWarp struct {
	server rpc.Daemon_DisconnectServer
}

func (r *disconnectWarp) Write(p []byte) (n int, err error) {
	err = r.server.Send(&rpc.DisconnectResponse{
		Message: string(p),
	})
	return len(p), err
}

func newDisconnectWarp(server rpc.Daemon_DisconnectServer) io.Writer {
	return &disconnectWarp{server: server}
}
