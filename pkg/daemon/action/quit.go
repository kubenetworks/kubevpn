package action

import (
	"io"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/dns"
)

func (svr *Server) Quit(req *rpc.QuitRequest, resp rpc.Daemon_QuitServer) error {
	defer func() {
		log.SetOutput(svr.LogFile)
		log.SetLevel(log.DebugLevel)
	}()
	log.SetOutput(io.MultiWriter(newQuitWarp(resp), svr.LogFile))
	log.SetLevel(log.InfoLevel)
	if svr.connect != nil {
		log.Info("quit: cleanup connection")
		svr.connect.Cleanup()
	}
	if svr.Cancel != nil {
		svr.Cancel()
	}
	if svr.clone != nil {
		log.Info("quit: cleanup clone")
		err := svr.clone.Cleanup()
		if err != nil {
			log.Errorf("quit: cleanup clone failed: %v", err)
		}
	}
	for _, options := range svr.secondaryConnect {
		log.Info("quit: cleanup connection")
		options.Cleanup()
	}

	dns.CleanupHosts()

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
