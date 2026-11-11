package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/grpcutil"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

const currentConfigSchemaVersion = 1

// Config represents the persisted daemon configuration containing secondary connections to restore on startup.
type Config struct {
	SchemaVersion    int                 `json:"SchemaVersion,omitempty"`
	SecondaryConnect []*connectionRecord `json:"SecondaryConnect"`
}

// LoadFromConfig reads persisted connection state from disk and re-establishes previously active connections.
func (svr *Server) LoadFromConfig(ctx context.Context) error {
	content, err := os.ReadFile(config.GetDBPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	jsonConf, err := yaml.YAMLToJSON(content)
	if err != nil {
		return err
	}
	var conf Config
	if err = json.Unmarshal(jsonConf, &conf); err != nil {
		return err
	}
	if conf.SchemaVersion < 0 || conf.SchemaVersion > currentConfigSchemaVersion {
		return fmt.Errorf("unsupported persisted schema version %d", conf.SchemaVersion)
	}
	if len(conf.SecondaryConnect) == 0 {
		return nil
	}
	var client rpc.DaemonClient
	for ctx.Err() == nil {
		_, err = svr.GetClient(true)
		if err != nil {
			if err = waitForDaemonRetry(ctx); err != nil {
				return err
			}
			continue
		}
		client, err = svr.GetClient(false)
		if err != nil {
			if err = waitForDaemonRetry(ctx); err != nil {
				return err
			}
			continue
		}
		break
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	restoreCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs(connectionRestoreMetadataKey, "true"))
	var restoreErrors []error
	for i, c := range conf.SecondaryConnect {
		if c == nil || len(c.RequestRaw) == 0 {
			continue
		}
		var req rpc.ConnectRequest
		if err = proto.Unmarshal(c.RequestRaw, &req); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("decode connection %d: %w", i, err))
			continue
		}
		var resp rpc.Daemon_ConnectClient
		resp, err = client.Connect(restoreCtx)
		if err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("connect connection %d: %w", i, err))
			continue
		}
		// Restore OwnerID from persisted ConnectOptions so the reconnect
		// reuses the same owner identity, preventing orphaned envoy rules.
		if c.OwnerID != "" {
			req.OwnerID = c.OwnerID
		}
		if err = resp.Send(&req); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("send connection %d: %w", i, err))
			continue
		}
		if err = grpcutil.PrintGRPCStream[rpc.ConnectResponse](ctx, resp, svr.LogFile); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("restore connection %d: %w", i, err))
		}
	}
	return errors.Join(restoreErrors...)
}

// RestoreAndReconcile rebuilds the authoritative user registry before reaping
// orphaned root sessions. Incomplete restores must never trigger destructive cleanup.
func (svr *Server) RestoreAndReconcile(ctx context.Context) {
	if err := svr.LoadFromConfig(ctx); err != nil {
		plog.G(ctx).Errorf("Failed to restore persisted connections; skipping root reconciliation: %v", err)
		return
	}
	svr.ReconcileSudoConnections(ctx)
}

func waitForDaemonRetry(ctx context.Context) error {
	timer := time.NewTimer(config.DaemonPollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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

	connections, _ := svr.registry.snapshot()
	userIDs := make(map[string]struct{}, len(connections))
	for _, c := range connections {
		userIDs[c.GetConnectionID()] = struct{}{}
	}

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
	if svr.IsSudo {
		return nil
	}
	svr.persistMu.Lock()
	defer svr.persistMu.Unlock()

	connections, _ := svr.registry.snapshot()
	conns := make([]*connectionRecord, 0, len(connections))
	for _, c := range connections {
		if co, ok := c.(*handler.ConnectOptions); ok {
			conns = append(conns, recordConnection(co))
		}
	}

	conf := &Config{
		SchemaVersion:    currentConfigSchemaVersion,
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
	return writeFileAtomically(config.GetDBPath(), yamlConf, config.FileModePrivateFile)
}

// CleanupConfig removes the user daemon's persisted configuration file from disk.
func (svr *Server) CleanupConfig() error {
	if svr.IsSudo {
		return nil
	}
	svr.persistMu.Lock()
	defer svr.persistMu.Unlock()
	return os.Remove(config.GetDBPath())
}

var renameFile = os.Rename

// writeFileAtomically replaces path only after a complete private temporary file
// has been written and synced in the same directory.
func writeFileAtomically(path string, data []byte, mode os.FileMode) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tempPath := f.Name()
	defer func() {
		_ = f.Close()
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if err = f.Chmod(mode); err != nil {
		return fmt.Errorf("set temporary file permissions: %w", err)
	}
	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err = renameFile(tempPath, path); err != nil {
		return fmt.Errorf("replace persisted configuration: %w", err)
	}
	if err = syncDirectory(dir); err != nil {
		return fmt.Errorf("sync configuration directory: %w", err)
	}
	return nil
}

type tunIP struct {
	v4, v6 string
	// status is the data-plane verdict computed by the sudo daemon (it owns the TUN and
	// observes heartbeat replies). The user daemon reuses it instead of recomputing.
	status string
}

// probeSudo queries the sudo daemon's Status (bounded by config.SudoStatusTimeout) and returns
// the per-connection data-plane state (TUN IPs + computed status) it owns, together with any
// error. The error is surfaced — not swallowed — so callers can distinguish a genuinely-down
// daemon (config.ErrDaemonNotRunning: socket missing / health gate failed) from a transient
// RPC failure (e.g. the sudo daemon restarting). On a connection-level error (codes.Unavailable)
// the stale cached client is dropped so the next probe redials and re-runs the health gate.
func (svr *Server) probeSudo(ctx context.Context) (map[string]tunIP, error) {
	if svr.GetClient == nil {
		return nil, nil
	}
	cli, err := svr.GetClient(true)
	if err != nil {
		return nil, err
	}
	// Bound the cross-daemon hop so a wedged sudo daemon can never stall an otherwise-local
	// user-daemon command (defense in depth beyond the cache-only read path). On timeout the
	// user daemon reports without the sudo-owned TUN IPs rather than blocking.
	ctx, cancel := context.WithTimeout(ctx, config.SudoStatusTimeout)
	defer cancel()
	resp, err := cli.Status(ctx, &rpc.StatusRequest{})
	if err != nil {
		if svr.InvalidateClient != nil && status.Code(err) == codes.Unavailable {
			svr.InvalidateClient(true)
		}
		return nil, err
	}
	m := make(map[string]tunIP, len(resp.GetList()))
	for _, s := range resp.GetList() {
		m[s.GetConnectionID()] = tunIP{
			v4:     s.GetIPv4(),
			v6:     s.GetIPv6(),
			status: s.GetStatus(),
		}
	}
	return m, nil
}

// getSudoTunIPs is the map-only convenience wrapper over probeSudo used by on-demand status
// reads. Unlike the old implementation it logs (rather than silently swallows) probe failures,
// returning nil so callers fall back to their local view.
func (svr *Server) getSudoTunIPs(ctx context.Context) map[string]tunIP {
	m, err := svr.probeSudo(ctx)
	if err != nil {
		plog.G(ctx).Warnf("Probe sudo daemon status failed: %v", err)
	}
	return m
}

// sudoProcessAlive is the crash-safe PID pre-check used by the liveness monitor. It is a var so
// tests can stub the process check without a real root-daemon PID file.
var sudoProcessAlive = sudoProcessAliveImpl

// MonitorSudoLiveness periodically probes the root (sudo) daemon and caches a per-connection
// health snapshot the user daemon serves from Status/ConnectionList. It is the peer-liveness
// edge the dual-daemon design otherwise lacked: after Connect returns there is no long-lived
// stream between the two daemons, so without this a crashed root daemon is only noticed lazily
// on the next CLI call. It REPORTS only — recovery (respawning the root daemon) needs
// interactive privilege escalation and is deferred to the next CLI command's StartupDaemon.
// User daemon only.
func (svr *Server) MonitorSudoLiveness(ctx context.Context) {
	if svr.IsSudo {
		return
	}
	svr.healthMu.Lock()
	svr.sudoHealthy = true // start optimistic so only genuine transitions are logged
	svr.healthMu.Unlock()

	ticker := time.NewTicker(config.SudoLivenessProbeInterval)
	defer ticker.Stop()
	for {
		svr.refreshSudoHealth(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// refreshSudoHealth performs one liveness probe and updates the cached snapshot.
func (svr *Server) refreshSudoHealth(ctx context.Context) {
	// Cheap crash-safe pre-check: a dead root-daemon process means the data plane is gone, so
	// report disconnected without a doomed gRPC dial (works for kill -9, unlike a stale socket).
	if !sudoProcessAlive() {
		svr.storeSudoHealth(ctx, map[string]tunIP{}, false, "root daemon process not running")
		return
	}
	m, err := svr.probeSudo(ctx)
	if err == nil {
		svr.storeSudoHealth(ctx, m, true, "")
		return
	}
	if errors.Is(err, config.ErrDaemonNotRunning) {
		svr.storeSudoHealth(ctx, map[string]tunIP{}, false, err.Error())
		return
	}
	// Transient failure (daemon alive but not answering, e.g. mid-restart): keep the last-known
	// connections but downgrade them to unhealthy rather than flapping straight to disconnected.
	svr.degradeSudoHealth(ctx, err)
}

// storeSudoHealth replaces the cached snapshot, logging only on a reachability transition to
// avoid a log line every probe interval.
func (svr *Server) storeSudoHealth(ctx context.Context, m map[string]tunIP, healthy bool, reason string) {
	svr.healthMu.Lock()
	was := svr.sudoHealthy
	svr.sudoHealth = m
	svr.sudoHealthy = healthy
	svr.healthMu.Unlock()
	switch {
	case was && !healthy:
		plog.G(ctx).Warnf("Root daemon liveness lost (%s); connections reported unhealthy/disconnected until it is restarted by the next CLI command", reason)
	case !was && healthy:
		plog.G(ctx).Infof("Root daemon liveness restored")
	}
}

// degradeSudoHealth downgrades every cached connection to unhealthy on a transient probe error.
func (svr *Server) degradeSudoHealth(ctx context.Context, cause error) {
	svr.healthMu.RLock()
	degraded := make(map[string]tunIP, len(svr.sudoHealth))
	for id, s := range svr.sudoHealth {
		s.status = StatusUnhealthy
		degraded[id] = s
	}
	svr.healthMu.RUnlock()
	svr.storeSudoHealth(ctx, degraded, false, "transient probe failure: "+cause.Error())
}

// sudoHealthSnapshot returns the cached per-connection sudo data-plane state for the user
// daemon's status reads. It falls back to a live probe only while the cache is still cold
// (before MonitorSudoLiveness has produced its first snapshot).
func (svr *Server) sudoHealthSnapshot(ctx context.Context) map[string]tunIP {
	svr.healthMu.RLock()
	cached := svr.sudoHealth
	svr.healthMu.RUnlock()
	if cached == nil {
		return svr.getSudoTunIPs(ctx)
	}
	cp := make(map[string]tunIP, len(cached))
	for k, v := range cached {
		cp[k] = v
	}
	return cp
}
