package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
)

func GetClient(isSudo bool) rpc.DaemonClient {
	sudo := ""
	if isSudo {
		sudo = "sudo"
	}
	ctx := context.Background()
	conn, err := grpc.DialContext(ctx, "passthrough:///unix://"+GetPortPath(isSudo), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Errorf("cannot connect to %s server: %v", sudo, err)
		fmt.Println(fmt.Errorf("cannot connect to %s server: %v", sudo, err))
		return nil
	}
	c := rpc.NewDaemonClient(conn)
	ctx, cancelFunc := context.WithTimeout(context.Background(), time.Millisecond*200)
	defer cancelFunc()
	now := time.Now()
	healthClient := grpc_health_v1.NewHealthClient(conn)
	response, err := healthClient.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		log.Printf("%v", err)
		return nil
	}
	fmt.Println(response.Status, sudo, time.Now().Sub(now).String())
	now = time.Now()
	_, err = c.Status(context.Background(), &rpc.StatusRequest{})
	fmt.Printf("call %s api status use %s\n", sudo, time.Now().Sub(now))
	if err != nil {
		fmt.Println(fmt.Errorf("cannot call %s api status: %v", sudo, err))
		log.Error(err)
		return nil
	}
	return c
}

type Apply interface {
	Apply(f func(cli rpc.DaemonClient)) error
}

type apply struct {
	err error
}

func (a *apply) Apply(f func(cli rpc.DaemonClient)) error {
	if a.err != nil {
		return a.err
	}

	return nil
}

func GetClients(isSudo bool) Apply {
	a := &apply{}
	p, err := os.ReadFile(GetPortPath(isSudo))
	if err != nil {
		a.err = err
		return a
	}
	port := strings.TrimSpace(string(p))
	ctx, cancelFunc := context.WithTimeout(context.Background(), time.Second*2)
	defer cancelFunc()
	var lc net.ListenConfig
	listen, err := lc.Listen(ctx, "tcp", fmt.Sprintf("localhost:%s", port))
	if err == nil {
		_ = listen.Close()
		return nil
	}

	conn, err := grpc.DialContext(ctx, fmt.Sprintf("localhost:%s", port), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Errorf("cannot connect to server: %v", err)
		return nil
	}
	c := rpc.NewDaemonClient(conn)
	_, err = c.Status(ctx, &rpc.StatusRequest{})
	if err != nil {
		return nil
	}
	return a
}

func GetPortPath(isSudo bool) string {
	name := config.PortPath
	if isSudo {
		name = config.SudoPortPath
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

func GetDaemonCommand(isSudo bool) *exec.Cmd {
	if isSudo {
		return exec.Command("sudo", "--preserve-env", os.Args[0], "daemon", "--sudo")
	}
	return exec.Command(os.Args[0], "daemon")
}
