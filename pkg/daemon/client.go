package daemon

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
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

func GetDaemonCommand(isSudo bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	fmt.Println(exe)
	if isSudo {
		return util.RunCmdWithElevated([]string{"daemon", "--sudo"})
	}
	err = util.RunCmd([]string{"daemon"})
	//cmd := exec.Command(exe, "daemon")
	//cmd.Stdout = os.Stdout
	//cmd.Stdin = os.Stdin
	//cmd.Stderr = os.Stderr
	//cmd.SysProcAttr = &syscall.SysProcAttr{
	//	HideWindow: true,
	//}
	//err = cmd.Start()
	//if err != nil {
	//	return err
	//}
	//go func() {
	//	err := cmd.Wait()
	//	if err != nil {
	//		log.Error(err)
	//	}
	//}()
	return err
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
				p.Wait()
			}
		}
	}
	err = GetDaemonCommand(isSudo)
	if err != nil {
		return err
	}
	//cmd.Stdin = os.Stdin
	//cmd.Stdout = os.Stdout
	//cmd.Stderr = os.Stderr
	//if err = cmd.Start(); err != nil {
	//	return err
	//}
	//err = os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), os.ModePerm)
	//if err != nil {
	//	return err
	//}
	//go func() {
	//	cmd.Wait()
	//}()

	for ctx.Err() == nil {
		time.Sleep(time.Millisecond * 50)
		//_ = os.Chmod(sockPath, os.ModeSocket)
		if _, err = os.Stat(sockPath); err == nil || errors.Is(err, windows.ERROR_CANT_ACCESS_FILE) {
			break
		}
	}
	err = os.Chmod(GetPidPath(isSudo), os.ModePerm)
	if err != nil {
		return err
	}

	client := GetClient(isSudo)
	if client == nil {
		return fmt.Errorf("can not get daemon server client")
	}
	_, err = client.Status(ctx, &rpc.StatusRequest{})

	return err
}
