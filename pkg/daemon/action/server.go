package action

import (
	"context"
	"sync"

	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

// Server implements the gRPC Daemon service. Session membership belongs to registry;
// resource setup and cleanup run outside its lock.
type Server struct {
	rpc.UnimplementedDaemonServer
	Cancel func()
	// GetClient returns a cached client for the peer daemon.
	GetClient func(isSudo bool) (rpc.DaemonClient, error)
	// InvalidateClient drops a failed peer client; it may be nil in tests.
	InvalidateClient func(isSudo bool)
	IsSudo           bool
	LogFile          *lumberjack.Logger

	// dnsMu serializes DNS setup/teardown across data sessions.
	dnsMu    sync.Mutex
	registry connectionRegistry
	// connectMu serializes connect decisions through cleanup, setup and publication.
	connectMu sync.Mutex
	// newDataSession substitutes privileged setup in daemon integration tests.
	newDataSession func(*handler.DataSession) handler.DataPlane

	// SSH server state is independent of DNS and connection membership.
	sshMu         sync.Mutex
	sshServerIP   string
	sshCancelFunc context.CancelFunc

	// User daemon's cached root-daemon liveness snapshot.
	healthMu    sync.RWMutex
	sudoHealth  map[string]tunIP
	sudoHealthy bool
	ID          string
}
