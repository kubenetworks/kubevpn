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
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/elevate"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/grpcutil"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

var (
	clientMu         sync.Mutex
	daemonClient     rpc.DaemonClient
	sudoDaemonClient rpc.DaemonClient
)

// GetClient returns a cached gRPC DaemonClient for the given privilege level, creating one if necessary.
func GetClient(isSudo bool) (cli rpc.DaemonClient, err error) {
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

	ctx := context.Background()
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
	var response *grpc_health_v1.HealthCheckResponse
	response, err = healthClient.Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		return nil, err
	}
	if response.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		return nil, fmt.Errorf("health check failed: %v: %w", response.Status, config.ErrDaemonNotRunning)
	}

	cli = rpc.NewDaemonClient(conn)
	var resp *rpc.UpgradeResponse
	resp, err = cli.Upgrade(ctx, &rpc.UpgradeRequest{
		ClientVersion: config.Version,
	})
	if err == nil && resp.NeedUpgrade {
		// quit daemon
		var quitStream rpc.Daemon_QuitClient
		quitStream, err = cli.Quit(ctx)
		if err != nil {
			return nil, err
		}
		err = quitStream.Send(&rpc.QuitRequest{Level: plog.GetLogLevel()})
		if err != nil {
			return nil, err
		}
		_ = grpcutil.PrintGRPCStream[rpc.QuitResponse](ctx, quitStream, nil)
		// The daemon is outdated and was asked to quit; do not return or cache a
		// client to it. Returning nil makes StartupDaemon spawn a fresh daemon.
		return nil, fmt.Errorf("daemon version is outdated and was asked to quit: %w", config.ErrDaemonVersionMismatch)
	}

	clientMu.Lock()
	if isSudo {
		sudoDaemonClient = cli
	} else {
		daemonClient = cli
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
	conn, err = grpc.NewClient("unix:"+sockPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
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
	if cli, _ := GetClient(false); cli == nil {
		if err = runDaemon(ctx, exe, false); err != nil {
			return err
		}
	}

	// sudo daemon (see ordering note above)
	if cli, _ := GetClient(true); cli == nil {
		if err = runDaemon(ctx, exe, true); err != nil {
			return err
		}
	}
	return nil
}

func runDaemon(ctx context.Context, exe string, isSudo bool) error {
	_, err := GetClient(isSudo)
	if err == nil {
		return nil
	}

	pidPath := config.GetPidPath(isSudo)
	err = os.Remove(pidPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if isSudo {
		if !elevate.IsAdmin() {
			err = elevate.RunCmdWithElevated(exe, []string{"daemon", "--sudo"})
		} else {
			err = elevate.RunCmd(exe, []string{"daemon", "--sudo"})
		}
	} else {
		err = elevate.RunCmd(exe, []string{"daemon"})
	}
	if err != nil {
		return err
	}
	const pidPollInterval = 50 * time.Millisecond
	for ctx.Err() == nil {
		time.Sleep(pidPollInterval)
		if _, err = os.Stat(pidPath); !errors.Is(err, os.ErrNotExist) {
			break
		}
	}

	_, err = GetClient(isSudo)
	if err != nil {
		return fmt.Errorf("failed to get daemon server client: %w", err)
	}
	return nil
}

// GetTCPClient dials the daemon Unix socket and returns a raw net.Conn for HTTP/TCP traffic.
func GetTCPClient(isSudo bool) net.Conn {
	conn, err := net.Dial("unix", config.GetSockPath(isSudo))
	if err != nil {
		return nil
	}
	return conn
}
