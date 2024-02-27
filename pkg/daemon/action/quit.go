package action

import (
	"io"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
)

func (svr *Server) Quit(req *rpc.QuitRequest, resp rpc.Daemon_QuitServer) error {
	defer func() {
		log.SetOutput(svr.LogFile)
		log.SetLevel(log.DebugLevel)
	}()
	log.SetOutput(io.MultiWriter(newQuitWarp(resp), svr.LogFile))
	log.SetLevel(log.InfoLevel)

	for i := len(svr.secondaryConnect) - 1; i >= 0; i-- {
		log.Info("quit: cleanup connection")
		svr.secondaryConnect[i].Cleanup()
	}

	if svr.connect != nil {
		log.Info("quit: cleanup connection")
		svr.connect.Cleanup()
	}
	if svr.clone != nil {
		log.Info("quit: cleanup clone")
		err := svr.clone.Cleanup()
		if err != nil {
			log.Errorf("quit: cleanup clone failed: %v", err)
		}
	}

	dns.CleanupHosts()

	// last step is to quit GRPC server
	if svr.Cancel != nil {
		svr.Cancel()
	}
	return nil
}

type quitWarp struct {
	server rpc.Daemon_QuitServer
}

func (r *quitWarp) Write(p []byte) (n int, err error) {
	err = r.server.Send(&rpc.QuitResponse{
		Message: string(p),
	})
	return len(p), err
}

func newQuitWarp(server rpc.Daemon_QuitServer) io.Writer {
	return &quitWarp{server: server}
}
