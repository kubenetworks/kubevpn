package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	_ "google.golang.org/grpc/resolver/dns"
	_ "google.golang.org/grpc/resolver/passthrough"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

var daemonClient, sudoDaemonClient rpc.DaemonClient

func GetClient(isSudo bool) (cli rpc.DaemonClient) {
	sockPath := config.GetSockPath(isSudo)
	if _, err := os.Stat(sockPath); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if isSudo && sudoDaemonClient != nil {
		return sudoDaemonClient
	}
	if !isSudo && daemonClient != nil {
		return daemonClient
	}

	ctx := context.Background()
	conn, err := grpc.DialContext(ctx, "unix:"+sockPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil
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
		return nil
	}
	if response.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		return nil
	}

	cli = rpc.NewDaemonClient(conn)
	var resp *rpc.UpgradeResponse
	resp, err = cli.Upgrade(ctx, &rpc.UpgradeRequest{
		ClientVersion: config.Version,
	})
	if err == nil && resp.NeedUpgrade {
		// quit daemon
		var quitStream rpc.Daemon_QuitClient
		quitStream, err = cli.Quit(ctx, &rpc.QuitRequest{})
		if err != nil {
			return nil
		}
		for {
			if _, err = quitStream.Recv(); err != nil {
				return nil
			}
		}
	}

	if isSudo {
		sudoDaemonClient = cli
	} else {
		daemonClient = cli
	}
	return cli
}

func GetClientWithoutCache(ctx context.Context, isSudo bool) (cli rpc.DaemonClient, conn *grpc.ClientConn, err error) {
	sockPath := config.GetSockPath(isSudo)
	_, err = os.Stat(sockPath)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	conn, err = grpc.DialContext(ctx, "unix:"+sockPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
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
		return
	}
	cli = rpc.NewDaemonClient(conn)
	return cli, conn, nil
}

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
	// normal daemon
	if daemonClient = GetClient(false); daemonClient == nil {
		if err = runDaemon(ctx, exe, false); err != nil {
			return err
		}
	}

	// sudo daemon
	if sudoDaemonClient = GetClient(true); sudoDaemonClient == nil {
		if err = runDaemon(ctx, exe, true); err != nil {
			return err
		}
	}
	return nil
}

func runDaemon(ctx context.Context, exe string, isSudo bool) error {
	cli := GetClient(isSudo)
	if cli != nil {
		return nil
	}

	pidPath := config.GetPidPath(isSudo)
	err := os.Remove(pidPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if isSudo {
		if !util.IsAdmin() {
			err = util.RunCmdWithElevated(exe, []string{"daemon", "--sudo"})
		} else {
			err = util.RunCmd(exe, []string{"daemon", "--sudo"})
		}
	} else {
		err = util.RunCmd(exe, []string{"daemon"})
	}
	if err != nil {
		return err
	}
	for ctx.Err() == nil {
		time.Sleep(time.Millisecond * 50)
		//_ = os.Chmod(sockPath, os.ModeSocket)
		if _, err = os.Stat(pidPath); !errors.Is(err, os.ErrNotExist) {
			break
		}
	}

	client := GetClient(isSudo)
	if client == nil {
		return fmt.Errorf("can not get daemon server client")
	}
	return nil
}

func GetHttpClient(isSudo bool) *http.Client {
	client := http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				var d net.Dialer
				d.Timeout = 30 * time.Second
				d.KeepAlive = 30 * time.Second
				return d.DialContext(ctx, "unix", config.GetSockPath(isSudo))
			},
		},
	}
	return &client
}

func GetTCPClient(isSudo bool) net.Conn {
	conn, err := net.Dial("unix", config.GetSockPath(isSudo))
	if err != nil {
		return nil
	}
	return conn
}
