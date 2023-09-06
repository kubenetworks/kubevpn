package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	_ "google.golang.org/grpc/resolver/dns"
	_ "google.golang.org/grpc/resolver/passthrough"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func GetClient(isSudo bool) rpc.DaemonClient {
	p, err := os.ReadFile(GetPortPath(isSudo))
	if err != nil {
		return nil
	}
	port := strings.TrimSpace(string(p))
	listen, err := net.Listen("tcp", fmt.Sprintf(":%s", port))
	if err == nil {
		_ = listen.Close()
		return nil
	}
	ctx := context.Background()
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
	return c
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
