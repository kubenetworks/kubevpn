package action

import (
	"context"
	"io"
	"sync"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
)

// streamWriter adapts a gRPC streaming Send call into an io.Writer.
// All daemon action handlers share this pattern to pipe log output
// to the client via their respective streaming response types.
//
// A send error means the gRPC stream is broken (client disconnected, daemon
// restarted, etc.). Reporting it would let an io.Writer consumer short-circuit —
// but logrus's StreamHook ignores Write errors, so surfacing it gains nothing
// while risking a noisy panic loop. Instead the failure is logged once (guarded
// by sync.Once so a dead stream does not spam on every log line), and Write
// reports success so logging continues to the file-backed side of the logger.
type streamWriter struct {
	send     func(msg string) error
	once     sync.Once
	logFail  func(error)
}

func (w *streamWriter) Write(p []byte) (int, error) {
	if err := w.send(string(p)); err != nil {
		// Report the first send failure only; subsequent writes against a dead
		// stream will keep failing silently to avoid log storms.
		w.once.Do(func() {
			if w.logFail != nil {
				w.logFail(err)
			}
		})
	}
	return len(p), nil
}

func newStreamWriter(send func(string) error) io.Writer {
	return &streamWriter{
		send:    send,
		logFail: func(err error) { plog.G(context.Background()).Warnf("stream log send failed (client stream closed?): %v", err) },
	}
}

// LogFieldConnID is the context field key used to tag every log line with the
// connection it belongs to (rendered as "[connID=xxxx]" by the server format).
// It lets concurrent operations be told apart in the shared daemon log file.
const LogFieldConnID = "connID"

// newServerStreamLogger builds a server-format logger for one RPC whose level FOLLOWS the
// request's log level (streamLevel = req.Level: Info by default, Debug when the client passed
// --debug). Both the log file and the StreamHook (message-only → gRPC stream → CLI) use that
// level, so a connection's whole footprint — file, stream, per-packet, and the background
// data-plane tasks it spawns (they run on this logger's ctx) — is recorded at the level the
// user asked for. A non-debug connection therefore neither floods the file with per-packet lines
// nor pays the formatting cost (IsDebugEnabled(ctx) is false).
func newServerStreamLogger(out io.Writer, streamLevel int32, sendMsg func(string) error) *log.Logger {
	// A zero streamLevel is logrus PanicLevel, which would suppress almost everything. Treat it as
	// Info — it means the caller's request carried no Level (e.g. an older client, or an RPC whose
	// request predates the Level field).
	if streamLevel == 0 {
		streamLevel = int32(log.InfoLevel)
	}
	logger := plog.GetLoggerForServer(streamLevel, out)
	logger.AddHook(&plog.StreamHook{
		Writer: newStreamWriter(sendMsg),
		Level:  log.Level(streamLevel),
	})
	return logger
}

// initStreamLogger creates a server-format logger writing to the log file,
// with a StreamHook that sends message-only text to the gRPC stream.
// Log file gets full debug info (timestamp+file:line); CLI gets simple messages
// filtered to streamLevel.
func (svr *Server) initStreamLogger(resp grpc.ServerStream, streamLevel int32, sendMsg func(string) error) (*log.Logger, context.Context) {
	logger := newServerStreamLogger(svr.LogFile, streamLevel, sendMsg)
	return logger, plog.WithLogger(resp.Context(), logger)
}

// resolveKubeconfigBytes resolves kubeconfig bytes from an RPC request WITHOUT
// writing a temp file. If an SSH jump host is configured it establishes the tunnel
// (kept alive for the lifetime of ctx) and returns the rewritten bytes pointing at
// the local endpoint; otherwise it returns the request bytes unchanged. Prefer
// this over resolveKubeconfig whenever the kubeconfig is consumed only by an
// in-process kubectl Factory (via util.InitFactoryByBytes) — it avoids the temp
// file entirely and the collision/permission/cleanup issues that come with it.
func resolveKubeconfigBytes(ctx context.Context, jump *rpc.SshJump, kubeconfigBytes string, portForward bool) ([]byte, error) {
	sshConf := parseSshFromRPC(jump)
	if !sshConf.IsEmpty() {
		return ssh.SshJumpBytes(ctx, sshConf, []byte(kubeconfigBytes), portForward)
	}
	return []byte(kubeconfigBytes), nil
}
