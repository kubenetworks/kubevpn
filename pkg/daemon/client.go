package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
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

func GetClient(isSudo bool) rpc.DaemonClient {
	if _, err := os.Stat(GetSockPath(isSudo)); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if isSudo && sudoDaemonClient != nil {
		return sudoDaemonClient
	}
	if !isSudo && daemonClient != nil {
		return daemonClient
	}

	ctx := context.Background()
	conn, err := grpc.DialContext(ctx, "unix:"+GetSockPath(isSudo), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil
	}
	cli := rpc.NewDaemonClient(conn)
	healthClient := grpc_health_v1.NewHealthClient(conn)
	var response *grpc_health_v1.HealthCheckResponse
	response, err = healthClient.Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		return nil
	}
	if response.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		return nil
	}
	_, err = cli.Status(ctx, &rpc.StatusRequest{})
	if err != nil {
		return nil
	}

	var resp *rpc.UpgradeResponse
	resp, err = cli.Upgrade(ctx, &rpc.UpgradeRequest{
		ClientVersion:  config.Version,
		ClientCommitId: config.GitCommit,
	})
	if err == nil && resp.NeedUpgrade {
		// quit daemon
		quitStream, err := cli.Quit(ctx, &rpc.QuitRequest{})
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

func GetSockPath(isSudo bool) string {
	name := config.SockPath
	if isSudo {
		name = config.SudoSockPath
	}
	return filepath.Join(config.DaemonPath, name)
}

func GetPidPath(isSudo bool) string {
	name := config.PidPath
	if isSudo {
		name = config.SudoPidPath
	}
	return filepath.Join(config.DaemonPath, name)
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
		if err := runDaemon(ctx, exe, false); err != nil {
			return err
		}
	}

	// sudo daemon
	if sudoDaemonClient = GetClient(true); sudoDaemonClient == nil {
		if err := runDaemon(ctx, exe, true); err != nil {
			return err
		}
	}
	return nil
}

func runDaemon(ctx context.Context, exe string, isSudo bool) error {
	sockPath := GetSockPath(isSudo)
	err := os.Remove(sockPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	pidPath := GetPidPath(isSudo)
	var file []byte
	if file, err = os.ReadFile(pidPath); err == nil {
		var pid int
		if pid, err = strconv.Atoi(strings.TrimSpace(string(file))); err == nil {
			var p *os.Process
			if p, err = os.FindProcess(pid); err == nil {
				if err = p.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
					log.Error("kill process", "err", err)
				}
				_, _ = p.Wait()
			}
		}
		err = os.Remove(pidPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
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
		if _, err = os.Stat(sockPath); !errors.Is(err, os.ErrNotExist) {
			break
		}
	}

	client := GetClient(isSudo)
	if client == nil {
		return fmt.Errorf("can not get daemon server client")
	}
	_, err = client.Status(ctx, &rpc.StatusRequest{})

	return err
}

func GetHttpClient(isSudo bool) *http.Client {
	client := http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				var d net.Dialer
				d.Timeout = 30 * time.Second
				d.KeepAlive = 30 * time.Second
				return d.DialContext(ctx, "unix", GetSockPath(isSudo))
			},
		},
	}
	return &client
}

func GetTCPClient(isSudo bool) net.Conn {
	conn, err := net.Dial("unix", GetSockPath(isSudo))
	if err != nil {
		return nil
	}
	return conn
}
