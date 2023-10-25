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
				break
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

	if req.GetAll() {
		if svr.connect != nil {
			svr.connect.Cleanup()
		}
		if svr.clone != nil {
			_ = svr.clone.Cleanup()
		}
		svr.t = time.Time{}
		svr.connect = nil
		svr.clone = nil

		for _, options := range svr.secondaryConnect {
			options.Cleanup()
		}
		svr.secondaryConnect = nil
	} else if req.ID != nil && req.GetID() == 0 {
		if svr.connect != nil {
			svr.connect.Cleanup()
		}
		if svr.clone != nil {
			_ = svr.clone.Cleanup()
		}
		svr.t = time.Time{}
		svr.connect = nil
		svr.clone = nil
	} else if req.ID != nil {
		index := req.GetID() - 1
		if index < int32(len(svr.secondaryConnect)) {
			svr.secondaryConnect[index].Cleanup()
			svr.secondaryConnect = append(svr.secondaryConnect[:index], svr.secondaryConnect[index+1:]...)
		} else {
			log.Errorf("index %d out of range", req.GetID())
		}
	}
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
