package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	_ "google.golang.org/grpc/resolver/dns"
	_ "google.golang.org/grpc/resolver/passthrough"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func GetClient(isSudo bool) rpc.DaemonClient {
	name := "daemon"
	if isSudo {
		name = "sudo_daemon"
	}
	port, err2 := os.ReadFile(filepath.Join(config.DaemonPortPath, name))
	if err2 != nil {
		return nil
	}
	ctx := context.Background()
	conn, err := grpc.DialContext(ctx, fmt.Sprintf("localhost:%s", string(port)), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Errorf("cannot connect to server: %v", err)
		return nil
	}
	return rpc.NewDaemonClient(conn)
}
