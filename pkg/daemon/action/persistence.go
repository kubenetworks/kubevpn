package action

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// Server implements the gRPC Daemon service, managing VPN connections, syncing, and lifecycle operations.
type Server struct {
	rpc.UnimplementedDaemonServer

	Cancel    func()
	GetClient func(isSudo bool) (rpc.DaemonClient, error)
	IsSudo    bool
	LogFile   *lumberjack.Logger
	Lock      sync.Mutex

	// connMu protects connections and currentConnectionID from concurrent access.
	// Use RLock for read-only access, Lock for mutations.
	connMu              sync.RWMutex
	currentConnectionID string
	connections         []*handler.ConnectOptions

	sshServerIP   string
	sshCancelFunc context.CancelFunc

	ID string
}

// Config represents the persisted daemon configuration containing secondary connections to restore on startup.
type Config struct {
	SecondaryConnect []*handler.ConnectOptions `json:"SecondaryConnect"`
}

// LoadFromConfig reads persisted connection state from disk and re-establishes previously active connections.
func (svr *Server) LoadFromConfig(ctx context.Context) error {
	content, err := os.ReadFile(config.GetDBPath())
	if err != nil {
		return err
	}
	jsonConf, err := yaml.YAMLToJSON(content)
	if err != nil {
		return err
	}
	var conf Config
	err = json.Unmarshal(jsonConf, &conf)
	if err != nil {
		return err
	}
	if len(conf.SecondaryConnect) == 0 {
		return nil
	}
	var client rpc.DaemonClient
	for ctx.Err() == nil {
		_, err = svr.GetClient(true)
		if err != nil {
			time.Sleep(config.DaemonPollInterval)
			continue
		}
		client, err = svr.GetClient(false)
		if err != nil {
			time.Sleep(config.DaemonPollInterval)
			continue
		}
		break
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	for _, c := range conf.SecondaryConnect {
		if c != nil {
			var resp rpc.Daemon_ConnectClient
			resp, err = client.Connect(context.Background())
			if err != nil {
				continue
			}
			c.Request.IPv4 = c.LocalTunIPv4.String()
			c.Request.IPv6 = c.LocalTunIPv6.String()
			err = resp.Send(c.Request)
			_ = util.PrintGRPCStream[rpc.ConnectResponse](nil, resp, svr.LogFile)
		}
	}
	return nil
}

// OffloadToConfig persists the current connection state to disk for later restoration.
func (svr *Server) OffloadToConfig() error {
	svr.connMu.RLock()
	conns := make([]*handler.ConnectOptions, len(svr.connections))
	copy(conns, svr.connections)
	svr.connMu.RUnlock()

	conf := &Config{
		SecondaryConnect: conns,
	}
	jsonConf, err := json.Marshal(conf)
	if err != nil {
		return err
	}
	yamlConf, err := yaml.JSONToYAML(jsonConf)
	if err != nil {
		return err
	}
	err = os.WriteFile(config.GetDBPath(), yamlConf, 0644)
	return err
}

// CleanupConfig removes the persisted configuration file from disk.
func (svr *Server) CleanupConfig() error {
	return os.Remove(config.GetDBPath())
}
