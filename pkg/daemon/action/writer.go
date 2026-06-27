package action

import (
	"context"
	"io"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// streamWriter adapts a gRPC streaming Send call into an io.Writer.
// All daemon action handlers share this pattern to pipe log output
// to the client via their respective streaming response types.
type streamWriter struct {
	send func(msg string) error
}

func (w *streamWriter) Write(p []byte) (int, error) {
	_ = w.send(string(p))
	return len(p), nil
}

func newStreamWriter(send func(string) error) io.Writer {
	return &streamWriter{send: send}
}

// initStreamLogger creates a server-format logger writing to the log file,
// with a StreamHook that sends message-only text to the gRPC stream.
// Log file gets full debug info (timestamp+file:line), CLI gets simple messages.
func (svr *Server) initStreamLogger(resp grpc.ServerStream, level int32, sendMsg func(string) error) (*log.Logger, context.Context) {
	logger := plog.GetLoggerForServer(level, svr.LogFile)
	logger.AddHook(&plog.StreamHook{
		Writer: newStreamWriter(sendMsg),
		Level:  log.InfoLevel,
	})
	return logger, plog.WithLogger(resp.Context(), logger)
}

// resolveKubeconfig resolves a kubeconfig file path from an RPC request.
// If an SSH jump host is configured, it tunnels through SSH first.
// The caller must defer os.Remove on the returned path.
func resolveKubeconfig(ctx context.Context, jump *rpc.SshJump, kubeconfigBytes string, portForward bool) (string, error) {
	sshConf := parseSshFromRPC(jump)
	if !sshConf.IsEmpty() {
		return ssh.SshJump(ctx, sshConf, []byte(kubeconfigBytes), portForward)
	}
	return util.ConvertToTempKubeconfigFile([]byte(kubeconfigBytes), "")
}
