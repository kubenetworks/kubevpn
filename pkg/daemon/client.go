package daemon

import (
	"context"
	"errors"
	"fmt"
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

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/util"
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

	sudo := ""
	if isSudo {
		sudo = "sudo"
	}
	ctx := context.Background()
	conn, err := grpc.DialContext(ctx, "unix:"+GetSockPath(isSudo), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Errorf("cannot connect to %s server: %v", sudo, err)
		fmt.Println(fmt.Errorf("cannot connect to %s server: %v", sudo, err))
		return nil
	}
	c := rpc.NewDaemonClient(conn)
	now := time.Now()
	healthClient := grpc_health_v1.NewHealthClient(conn)
	var response *grpc_health_v1.HealthCheckResponse
	response, err = healthClient.Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		log.Printf("%v", err)
		return nil
	}
	fmt.Println(response.Status, sudo, time.Now().Sub(now).String())
	now = time.Now()
	_, err = c.Status(ctx, &rpc.StatusRequest{})
	fmt.Printf("call %s api status use %s\n", sudo, time.Now().Sub(now))
	if err != nil {
		fmt.Println(fmt.Errorf("cannot call %s api status: %v", sudo, err))
		log.Error(err)
		return nil
	}
	if isSudo {
		sudoDaemonClient = c
	} else {
		daemonClient = c
	}
	return c
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

func StartupDaemon(ctx context.Context) error {
	// normal daemon
	if daemonClient = GetClient(false); daemonClient == nil {
		if err := runDaemon(ctx, false); err != nil {
			return err
		}
	}

	// sudo daemon
	if sudoDaemonClient = GetClient(true); sudoDaemonClient == nil {
		if err := runDaemon(ctx, true); err != nil {
			return err
		}
	}
	return nil
}

func runDaemon(ctx context.Context, isSudo bool) error {
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
				if err = p.Kill(); err != nil && err != os.ErrProcessDone {
					log.Error(err)
				}
				_, _ = p.Wait()
			}
		}
		err = os.Remove(pidPath)
		if err != nil {
			return err
		}
	}
	if isSudo {
		if !util.IsAdmin() {
			err = util.RunCmdWithElevated([]string{"daemon", "--sudo"})
		} else {
			err = util.RunCmd([]string{"daemon", "--sudo"})
		}
	} else {
		err = util.RunCmd([]string{"daemon"})
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
