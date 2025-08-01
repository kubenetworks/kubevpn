package action

import (
	"context"
	"io"
	"os"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) Quit(resp rpc.Daemon_QuitServer) error {
	defer svr.CleanupConfig()

	logger := plog.GetLoggerForClient(int32(log.InfoLevel), io.MultiWriter(newQuitWarp(resp), svr.LogFile))
	ctx := context.Background()
	if resp != nil {
		ctx = resp.Context()
	}
	ctx = plog.WithLogger(ctx, logger)

	connects := handler.Connects(svr.connections)
	for _, conn := range connects.Sort() {
		if conn != nil {
			if conn.Sync != nil {
				_ = conn.Sync.Cleanup(ctx)
			}
			conn.Cleanup(ctx)
		}
	}
	svr.connections = nil

	if svr.IsSudo {
		_ = dns.CleanupHosts()
		_ = os.RemoveAll("/etc/resolver")
	}

	// last step is to quit GRPC server
	if svr.Cancel != nil {
		svr.Cancel()
		svr.Cancel = nil
	}

	_ = util.CleanupTempKubeConfigFile()
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
