package action

import (
	"context"
	"os"

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
	var level int32
	if resp != nil {
		// QuitRequest carries the CLI's --debug intent; read it so the stream level matches
		// (newServerStreamLogger normalizes a zero/absent level to Info).
		if req, recvErr := resp.Recv(); recvErr == nil {
			level = req.GetLevel()
		}
		sendFunc = func(msg string) error { return resp.Send(&rpc.QuitResponse{Message: msg}) }
	} else {
		sendFunc = func(string) error { return nil }
	}
	logger := newServerStreamLogger(svr.LogFile, level, sendFunc)
	ctx := context.Background()
	if resp != nil {
		ctx = resp.Context()
	}
	ctx = plog.WithLogger(ctx, logger)

	svr.connMu.Lock()
	connects := handler.Connects(svr.connections)
	svr.connections = nil
	svr.connMu.Unlock()
	sorted := connects.Sort()
	// Emit the progress step only from the user daemon (the orchestrator). The sudo
	// daemon cleans up its data-plane connections silently so the CLI renders
	// "Cleaned up N connections" exactly once — see Disconnect for the same guard.
	if !svr.IsSudo && len(sorted) > 0 {
		plog.StepStart(ctx, "Cleaning up connections")
	}
	for _, conn := range sorted {
		cleanupConnection(ctx, conn)
	}
	if !svr.IsSudo && len(sorted) > 0 {
		plog.StepDone(ctx, "Cleaned up %d connections", len(sorted))
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
