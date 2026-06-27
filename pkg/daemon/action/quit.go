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

// Quit handles the Quit RPC, cleaning up all connections and stopping the daemon.
func (svr *Server) Quit(resp rpc.Daemon_QuitServer) error {
	defer svr.CleanupConfig()

	var sendFunc func(string) error
	if resp != nil {
		sendFunc = func(msg string) error { return resp.Send(&rpc.QuitResponse{Message: msg}) }
	} else {
		sendFunc = func(string) error { return nil }
	}
	logger := plog.GetLoggerForClient(int32(log.InfoLevel), io.MultiWriter(newStreamWriter(sendFunc), svr.LogFile))
	ctx := context.Background()
	if resp != nil {
		ctx = resp.Context()
	}
	ctx = plog.WithLogger(ctx, logger)

	svr.connMu.Lock()
	connects := handler.Connects(svr.connections)
	svr.connections = nil
	svr.connMu.Unlock()
	for _, conn := range connects.Sort() {
		cleanupConnection(ctx, conn)
	}

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

