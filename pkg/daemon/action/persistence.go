package action

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"gopkg.in/natefinch/lumberjack.v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/grpcutil"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
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
	connections         []handler.Connection

	// connectMu serializes the connect (TUN IP allocation) phase so two
	// concurrent connects to different clusters cannot race and receive the same
	// local TUN IP. Held in the root daemon around DoConnect.
	connectMu sync.Mutex

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
		if c != nil && len(c.RequestRaw) > 0 {
			var req rpc.ConnectRequest
			if err = proto.Unmarshal(c.RequestRaw, &req); err != nil {
				continue
			}
			var resp rpc.Daemon_ConnectClient
			resp, err = client.Connect(context.Background())
			if err != nil {
				continue
			}
			// Restore OwnerID from persisted ConnectOptions so the reconnect
			// reuses the same owner identity, preventing orphaned envoy rules.
			if c.OwnerID != "" {
				req.OwnerID = c.OwnerID
			}
			_ = resp.Send(&req)
			_ = grpcutil.PrintGRPCStream[rpc.ConnectResponse](ctx, resp, svr.LogFile)
		}
	}
	return nil
}

// ReconcileSudoConnections reaps orphaned sudo (data-plane) DataSessions left after daemon
// drift — e.g. the user daemon was restarted (or lost its persisted state) while the sudo
// daemon kept running with connections the user daemon no longer tracks. Such an orphan holds
// a phantom TUN, and (its cluster being unreachable) previously wedged commands via the
// status read path. Call once at startup, AFTER LoadFromConfig has re-established the
// authoritative set.
//
// User daemon only. To avoid racing an in-flight connect — forwardConnectToSudo registers the
// sudo DataSession BEFORE appending to the user slice — it reaps only sudo connections that
// are BOTH absent from the user daemon's set AND not reporting "connected": an establishing or
// healthy connection reports connected and is spared, while an orphan (unreachable cluster →
// dead heartbeat) reports unhealthy/disconnected. Best-effort.
func (svr *Server) ReconcileSudoConnections(ctx context.Context) {
	if svr.IsSudo || svr.GetClient == nil {
		return
	}
	cli, err := svr.GetClient(true)
	if err != nil {
		return
	}
	statusCtx, cancel := context.WithTimeout(ctx, config.SudoStatusTimeout)
	resp, err := cli.Status(statusCtx, &rpc.StatusRequest{})
	cancel()
	if err != nil {
		return
	}

	svr.connMu.RLock()
	userIDs := make(map[string]struct{}, len(svr.connections))
	for _, c := range svr.connections {
		userIDs[c.GetConnectionID()] = struct{}{}
	}
	svr.connMu.RUnlock()

	for _, s := range resp.GetList() {
		id := s.GetConnectionID()
		if id == "" {
			continue
		}
		if _, ok := userIDs[id]; ok {
			continue // tracked by the user daemon — not an orphan
		}
		if s.GetStatus() == StatusOk {
			continue // establishing or healthy — spare it (avoid racing an in-flight connect)
		}
		plog.G(ctx).Infof("Reaping orphaned data-plane connection %s (status=%s) with no user-daemon owner", id, s.GetStatus())
		if err := svr.reapSudoConnection(ctx, cli, id); err != nil {
			plog.G(ctx).Warnf("Failed to reap orphaned connection %s: %v", id, err)
		}
	}
}

// reapSudoConnection asks the sudo daemon to disconnect a single orphaned connection by ID.
// The sudo daemon's Disconnect handler (IsSudo) removes the connection and tears down its
// data plane directly — no leave/forward orchestration, which is correct for an orphan whose
// cluster is unreachable.
func (svr *Server) reapSudoConnection(ctx context.Context, cli rpc.DaemonClient, id string) error {
	stream, err := cli.Disconnect(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&rpc.DisconnectRequest{ConnectionID: ptr.To(id)}); err != nil {
		return err
	}
	return grpcutil.PrintGRPCStream[rpc.DisconnectResponse](ctx, stream, svr.LogFile)
}

// OffloadToConfig persists the current connection state to disk for later restoration.
func (svr *Server) OffloadToConfig() error {
	svr.connMu.RLock()
	conns := make([]*handler.ConnectOptions, 0, len(svr.connections))
	for _, c := range svr.connections {
		if co, ok := c.(*handler.ConnectOptions); ok {
			conns = append(conns, co)
		}
	}
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
	err = os.WriteFile(config.GetDBPath(), yamlConf, config.FileModeFile)
	return err
}

// CleanupConfig removes the persisted configuration file from disk.
func (svr *Server) CleanupConfig() error {
	return os.Remove(config.GetDBPath())
}

type tunIP struct {
	v4, v6 string
	// status is the data-plane verdict computed by the sudo daemon (it owns the TUN and
	// observes heartbeat replies). The user daemon reuses it instead of recomputing.
	status string
}

// getSudoTunIPs queries the sudo daemon and returns a map from ConnectionID to the data-plane
// state (TUN IPs + computed status) it owns.
func (svr *Server) getSudoTunIPs(ctx context.Context) map[string]tunIP {
	if svr.GetClient == nil {
		return nil
	}
	cli, err := svr.GetClient(true)
	if err != nil {
		return nil
	}
	// Bound the cross-daemon hop so a wedged sudo daemon can never stall an otherwise-local
	// user-daemon command (defense in depth beyond the cache-only read path). On timeout the
	// user daemon reports without the sudo-owned TUN IPs rather than blocking.
	ctx, cancel := context.WithTimeout(ctx, config.SudoStatusTimeout)
	defer cancel()
	resp, err := cli.Status(ctx, &rpc.StatusRequest{})
	if err != nil {
		return nil
	}
	m := make(map[string]tunIP, len(resp.GetList()))
	for _, s := range resp.GetList() {
		m[s.GetConnectionID()] = tunIP{
			v4:     s.GetIPv4(),
			v6:     s.GetIPv6(),
			status: s.GetStatus(),
		}
	}
	return m
}
