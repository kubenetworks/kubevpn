package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

var errDaemonAlreadyRunning = errors.New("daemon already running")

// acquireDaemonInstanceLock takes the role's non-blocking lifetime lock. The
// caller holds it until Serve returns; the OS releases it if the process dies.
func acquireDaemonInstanceLock(isSudo bool) (io.Closer, error) {
	lock, err := acquireProcessLock(context.Background(), config.GetSockPath(isSudo)+".lock", false)
	if errors.Is(err, errDaemonAlreadyRunning) {
		return nil, fmt.Errorf("%w (%s)", errDaemonAlreadyRunning, daemonRole(isSudo))
	}
	return lock, err
}

// acquireDaemonStartupLock serializes launch/readiness across CLI processes.
// It is deliberately separate from the lifetime instance lock.
func acquireDaemonStartupLock(ctx context.Context, isSudo bool) (io.Closer, error) {
	return acquireProcessLock(ctx, config.GetSockPath(isSudo)+".startup.lock", true)
}

func daemonRole(isSudo bool) string {
	if isSudo {
		return "root"
	}
	return "user"
}
