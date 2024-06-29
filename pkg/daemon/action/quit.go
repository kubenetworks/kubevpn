package action

import (
	"io"
	"os"
	"path/filepath"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

func (svr *Server) Quit(req *rpc.QuitRequest, resp rpc.Daemon_QuitServer) error {
	defer func() {
		log.SetOutput(svr.LogFile)
		log.SetLevel(log.DebugLevel)
	}()
	log.SetOutput(io.MultiWriter(newQuitWarp(resp), svr.LogFile))
	log.SetLevel(log.InfoLevel)

	if svr.clone != nil {
		log.Info("quit: cleanup clone")
		err := svr.clone.Cleanup()
		if err != nil {
			log.Errorf("quit: cleanup clone failed: %v", err)
		}
	}

	connects := handler.Connects(svr.secondaryConnect).Append(svr.connect)
	for _, conn := range connects.Sort() {
		log.Info("quit: cleanup connection")
		if conn != nil {
			conn.Cleanup()
		}
	}

	if svr.IsSudo {
		_ = dns.CleanupHosts()
		_ = os.RemoveAll(filepath.Join("/", "etc", "resolver"))
	}

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
	if r.server == nil {
		return len(p), nil
	}
	_ = r.server.Send(&rpc.QuitResponse{
		Message: string(p),
	})
	return len(p), nil
}

func newQuitWarp(server rpc.Daemon_QuitServer) io.Writer {
	return &quitWarp{server: server}
}
