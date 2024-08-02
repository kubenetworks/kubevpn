package action

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) Quit(req *rpc.QuitRequest, resp rpc.Daemon_QuitServer) error {
	defer func() {
		util.InitLoggerForServer(true)
		log.SetOutput(svr.LogFile)
		config.Debug = false
	}()
	util.InitLoggerForClient(config.Debug)
	log.SetOutput(io.MultiWriter(newQuitWarp(resp), svr.LogFile))

	if svr.clone != nil {
		err := svr.clone.Cleanup()
		if err != nil {
			log.Errorf("Cleanup clone failed: %v", err)
		}
		svr.clone = nil
	}

	connects := handler.Connects(svr.secondaryConnect).Append(svr.connect)
	for _, conn := range connects.Sort() {
		if conn != nil {
			conn.Cleanup()
		}
	}
	svr.secondaryConnect = nil
	svr.connect = nil

	if svr.IsSudo {
		_ = dns.CleanupHosts()
		_ = os.RemoveAll("/etc/resolver")
	}

	// last step is to quit GRPC server
	if svr.Cancel != nil {
		svr.Cancel()
		svr.Cancel = nil
	}

	_ = cleanupTempKubeConfigFile()
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

func cleanupTempKubeConfigFile() error {
	return filepath.Walk(os.TempDir(), func(path string, info fs.FileInfo, err error) error {
		if strings.HasSuffix(path, ".kubeconfig") {
			return os.Remove(path)
		}
		return err
	})
}
