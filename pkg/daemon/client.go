package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	_ "google.golang.org/grpc/resolver/dns"
	_ "google.golang.org/grpc/resolver/passthrough"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/grpcutil"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

var (
	clientMu         sync.Mutex
	daemonInitMu     sync.Mutex
	sudoDaemonInitMu sync.Mutex
	daemonClient     rpc.DaemonClient
	daemonConn       *grpc.ClientConn
	sudoDaemonClient rpc.DaemonClient
	sudoDaemonConn   *grpc.ClientConn
)

// InvalidateClient drops the cached client (and closes its connection) for the given
// privilege level, forcing the next GetClient to redial and re-run the health/version
// gate. Call this when an RPC on the cached client fails with a connection-level error
// (e.g. the peer daemon crashed or restarted), so a stale ClientConn is not reused
// indefinitely.
func InvalidateClient(isSudo bool) {
	clientMu.Lock()
	defer clientMu.Unlock()
	if isSudo {
		if sudoDaemonConn != nil {
			_ = sudoDaemonConn.Close()
			sudoDaemonConn = nil
		}
		sudoDaemonClient = nil
	} else {
		if daemonConn != nil {
			_ = daemonConn.Close()
			daemonConn = nil
		}
		daemonClient = nil
	}
}

// GetClient returns a cached gRPC DaemonClient for the given privilege level, creating one if necessary.
func GetClient(isSudo bool) (rpc.DaemonClient, error) {
	return getClientWithContext(context.Background(), isSudo)
}

func getClientWithContext(ctx context.Context, isSudo bool) (cli rpc.DaemonClient, err error) {
	sockPath := config.GetSockPath(isSudo)
	if _, err = os.Stat(sockPath); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %w", err, config.ErrDaemonNotRunning)
	}
	clientMu.Lock()
	if isSudo && sudoDaemonClient != nil {
		defer clientMu.Unlock()
		return sudoDaemonClient, nil
	}
	if !isSudo && daemonClient != nil {
		defer clientMu.Unlock()
		return daemonClient, nil
	}
	clientMu.Unlock()

	initMu := &daemonInitMu
	if isSudo {
		initMu = &sudoDaemonInitMu
	}
	initMu.Lock()
	defer initMu.Unlock()
	// Only one caller per role performs cold dial and version negotiation.
	clientMu.Lock()
	if isSudo && sudoDaemonClient != nil {
		cached := sudoDaemonClient
		clientMu.Unlock()
		return cached, nil
	}
	if !isSudo && daemonClient != nil {
		cached := daemonClient
		clientMu.Unlock()
		return cached, nil
	}
	clientMu.Unlock()

	conn, err := grpc.NewClient("unix:"+sockPath, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithNoProxy())
	if err != nil {
		return nil, err
	}
	defer func() {
		if cli == nil {
			_ = conn.Close()
		}
	}()

	healthClient := grpc_health_v1.NewHealthClient(conn)
	healthCtx, cancelHealth := context.WithTimeout(ctx, config.DaemonProbeTimeout)
	var response *grpc_health_v1.HealthCheckResponse
	response, err = healthClient.Check(healthCtx, &grpc_health_v1.HealthCheckRequest{})
	cancelHealth()
	if err != nil {
		return nil, err
	}
	if response.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		return nil, fmt.Errorf("health check failed: %v: %w", response.Status, config.ErrDaemonNotRunning)
	}

	cli = rpc.NewDaemonClient(conn)
	upgradeCtx, cancelUpgrade := context.WithTimeout(ctx, config.DaemonProbeTimeout)
	var resp *rpc.UpgradeResponse
	resp, err = cli.Upgrade(upgradeCtx, &rpc.UpgradeRequest{
		ClientVersion: config.Version,
	})
	cancelUpgrade()
	if err != nil {
		return nil, err
	}
	if resp.NeedUpgrade {
		// Cleanup can tear down multiple live connections, so it gets its own
		// longer deadline instead of the short health/version probe timeout.
		quitCtx, cancelQuit := context.WithTimeout(ctx, config.DaemonQuitTimeout)
		defer cancelQuit()
		var quitStream rpc.Daemon_QuitClient
		quitStream, err = cli.Quit(quitCtx)
		if err != nil {
			return nil, err
		}
		err = quitStream.Send(&rpc.QuitRequest{Level: plog.GetLogLevel()})
		if err != nil {
			return nil, err
		}
		if err = grpcutil.PrintGRPCStream[rpc.QuitResponse](quitCtx, quitStream, nil); err != nil {
			return nil, fmt.Errorf("failed to retire outdated daemon: %w", err)
		}
		// The daemon is outdated and was asked to quit; do not return or cache a
		// client to it. Returning nil makes StartupDaemon spawn a fresh daemon.
		return nil, fmt.Errorf("daemon version is outdated and was asked to quit: %w", config.ErrDaemonVersionMismatch)
	}

	clientMu.Lock()
	if isSudo && sudoDaemonClient != nil {
		cached := sudoDaemonClient
		clientMu.Unlock()
		_ = conn.Close()
		return cached, nil
	}
	if !isSudo && daemonClient != nil {
		cached := daemonClient
		clientMu.Unlock()
		_ = conn.Close()
		return cached, nil
	}
	if isSudo {
		sudoDaemonClient = cli
		sudoDaemonConn = conn
	} else {
		daemonClient = cli
		daemonConn = conn
	}
	clientMu.Unlock()
	return cli, nil
}

func getClientWithoutCache(ctx context.Context, isSudo bool) (cli rpc.DaemonClient, conn *grpc.ClientConn, err error) {
	sockPath := config.GetSockPath(isSudo)
	_, err = os.Stat(sockPath)
	if errors.Is(err, os.ErrNotExist) {
		err = fmt.Errorf("%w: %w", err, config.ErrDaemonNotRunning)
		return
	}
	conn, err = grpc.NewClient("unix:"+sockPath, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithNoProxy())
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			_ = conn.Close()
		}
	}()
	healthClient := grpc_health_v1.NewHealthClient(conn)
	var response *grpc_health_v1.HealthCheckResponse
	response, err = healthClient.Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		return
	}
	if response.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		err = fmt.Errorf("health check failed: %v: %w", response.Status, config.ErrDaemonNotRunning)
		return
	}
	cli = rpc.NewDaemonClient(conn)
	return cli, conn, nil
}

// StartupDaemon ensures both the normal and sudo daemon processes are running, starting them if needed.
func StartupDaemon(ctx context.Context, path ...string) error {
	var exe string
	var err error
	if len(path) != 0 {
		exe = path[0]
	} else {
		exe, err = os.Executable()
	}
	if err != nil {
		return err
	}
	// Ordering matters: the user (unprivileged) daemon MUST start before the sudo
	// daemon. runDaemon below blocks until the started daemon has written its PID,
	// and config.init() (which creates the ~/.kubevpn dir tree) runs at process
	// start — so by the time the sudo daemon spawns, the dirs already exist and are
	// owned by the unprivileged user. If the sudo daemon ran first it would create
	// them as root, and the user daemon could no longer write into them (EACCES).
	// Do NOT reorder these two blocks.
	if cli, probeErr := getClientWithContext(ctx, false); cli == nil {
		if err = runDaemonWithProbe(ctx, exe, false, probeErr); err != nil {
			return err
		}
	}

	// sudo daemon (see ordering note above)
	if cli, probeErr := getClientWithContext(ctx, true); cli == nil {
		if err = runDaemonWithProbe(ctx, exe, true, probeErr); err != nil {
			return err
		}
	}
	return nil
}

func runDaemon(ctx context.Context, exe string, isSudo bool) error {
	return runDaemonWithProbe(ctx, exe, isSudo, nil)
}

func runDaemonWithProbe(ctx context.Context, exe string, isSudo bool, initialProbeErr error) error {
	lockCtx, cancelLock := context.WithTimeout(ctx, config.DaemonStartupLockTimeout)
	startupLock, err := acquireDaemonStartupLock(lockCtx, isSudo)
	cancelLock()
	if err != nil {
		return fmt.Errorf("failed to acquire %s daemon startup lock: %w", daemonRole(isSudo), err)
	}
	defer startupLock.Close()
	// Another CLI may have completed startup while this process waited. The
	// probe is the only pre-launch request needed here; StartupDaemon already
	// performed the fast-path check before entering runDaemon.
	_, probeErr := getClientWithContext(ctx, isSudo)
	if probeErr == nil {
		return nil
	}

	role := daemonRole(isSudo)
	pidPath := config.GetPidPath(isSudo)
	if pidFileProcessAlive(pidPath) {
		// The second probe may see only Unavailable after the first probe has
		// already retired an outdated daemon. Preserve that initial version state.
		retiringVersion := errors.Is(probeErr, config.ErrDaemonVersionMismatch) || errors.Is(initialProbeErr, config.ErrDaemonVersionMismatch)
		if !retiringVersion {
			// A timeout or transient RPC error is not evidence that the process
			// is dead. Preserve its PID and socket instead of launching a duplicate.
			return fmt.Errorf("%s daemon is running but client probe failed: %w", role, probeErr)
		}
		exitCtx, cancelExit := context.WithTimeout(ctx, config.DaemonExitTimeout)
		defer cancelExit()
		if err = waitForDaemonProcessExit(exitCtx, pidPath); err != nil {
			return fmt.Errorf("failed to wait for outdated %s daemon to exit: %w", role, err)
		}
	}
	previousPID, err := readPIDFileState(pidPath)
	if err != nil {
		return fmt.Errorf("read %s daemon pid state: %w", role, err)
	}
	startupLogPath, err := startDaemonProcess(exe, isSudo)
	diagnostics := "log: " + config.GetDaemonLogPath(isSudo)
	if startupLogPath != "" {
		diagnostics = "startup log: " + startupLogPath + "; " + diagnostics
	}
	if err != nil {
		return fmt.Errorf("failed to launch %s daemon (%s): %w", role, diagnostics, err)
	}
	// Authentication has finished. Bound PID readiness separately from the
	// detached process lifetime and the bounded GetClient bootstrap probes.
	pidCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err = waitForDaemonPID(pidCtx, pidPath, previousPID); err != nil {
		return fmt.Errorf("failed to wait for %s daemon pid (%s): %w", role, diagnostics, err)
	}

	_, err = getClientWithContext(ctx, isSudo)
	if err != nil {
		return fmt.Errorf("failed to get %s daemon server client (%s): %w", role, diagnostics, err)
	}
	return nil
}

type pidFileState struct {
	content string
	modTime time.Time
}

func readPIDFileState(path string) (pidFileState, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return pidFileState{}, nil
	}
	if err != nil {
		return pidFileState{}, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return pidFileState{}, err
	}
	return pidFileState{content: string(content), modTime: info.ModTime()}, nil
}

func waitForDaemonPID(ctx context.Context, pidPath string, previous pidFileState) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, err := readPIDFileState(pidPath)
		if err != nil {
			return err
		}
		if current != previous && current.content != "" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func waitForDaemonProcessExit(ctx context.Context, pidPath string) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !pidFileProcessAlive(pidPath) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// GetTCPClient dials the daemon Unix socket and returns a raw net.Conn for HTTP/TCP traffic.
func GetTCPClient(isSudo bool) net.Conn {
	conn, err := net.Dial("unix", config.GetSockPath(isSudo))
	if err != nil {
		return nil
	}
	return conn
}
