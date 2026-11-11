package network

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"time"

	"tailscale.com/types/logger"

	"github.com/wencaiwulue/kubevpn/v2/pkg/cluster/portforward"
	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/endpoint"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tunnel"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
	gossh "golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// transportSupervisor owns transport publication, health policy, monitor lifetime and
// worker admission. Its callbacks are limited to route effects and read-only snapshots;
// it does not hold the Runtime or its mutable address/DNS/device state.
type transportSupervisor struct {
	cfg            *Config
	ctx            context.Context
	cancel         context.CancelFunc
	mu             sync.Mutex
	active         *transportGeneration
	changed        chan struct{}
	failures       chan transportEvent
	checks         chan uint64
	monitorChanges chan struct{}
	monitor        *networkMonitor
	newMonitor     func(logger.Logf) (networkMonitorSource, error)
	stopped        bool
	armed          bool
	err            error
	lastReply      func() time.Time
	heartbeatStats *tunnel.HeartbeatStats
	workers        sync.WaitGroup
	startupDone    chan struct{}
	addRoute       func(...string) error
	repairRoutes   func(context.Context)
	routeSnapshot  func() routeResolver
}

type transportKind uint8

const (
	transportPF transportKind = iota
	transportSSH
)

type transportEndpoint uint8

const (
	endpointTCP transportEndpoint = iota
	endpointUDP
	endpointXDS
)

type transportEndpoints struct{ tcp, udp, xds string }
type transportHandle interface {
	Stop()
	Done() <-chan struct{}
	Err() error
}

// All publication, retirement and returned-connection registration use mu.
// No network operation or blocking Close/Stop runs while that mutex is held.
type transportGeneration struct {
	id         uint64
	kind       transportKind
	stage      string // preparation stage, read only after prepareTransport returns
	diagnostic *transportDiagnostics
	ctx        context.Context
	cancel     context.CancelFunc
	endpoints  transportEndpoints
	handles    []transportHandle
	retired    bool
	retireOnce sync.Once
	conns      map[*generationConn]struct{}
	dials      sync.WaitGroup
	applyMu    sync.Mutex // serializes watcher effects with retirement, never mu
}
type transportEvent struct {
	generation uint64
	err        error
}
type generationConn struct {
	net.Conn
	nm         *transportSupervisor
	generation *transportGeneration
	once       sync.Once
	err        error
}

func (c *generationConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.nm.mu.Lock()
		delete(c.generation.conns, c)
		c.nm.mu.Unlock()
	})
	return c.err
}

type generationTransporter struct {
	nm       *transportSupervisor
	base     endpoint.Transporter
	endpoint transportEndpoint
}

func (t *generationTransporter) Dial(ctx context.Context, _ string) (net.Conn, error) {
	return t.nm.dialTransport(ctx, t.endpoint, t.base)
}

func (nm *transportSupervisor) waitTransport(ctx context.Context) (*transportGeneration, error) {
	for {
		nm.mu.Lock()
		g, changed, terminal, stopped := nm.active, nm.changed, nm.err, nm.stopped
		nm.mu.Unlock()
		if terminal != nil {
			return nil, terminal
		}
		if stopped {
			return nil, net.ErrClosed
		}
		if g != nil {
			return g, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}
func (nm *transportSupervisor) notifyTransportLocked() {
	close(nm.changed)
	nm.changed = make(chan struct{})
}
func (nm *transportSupervisor) publishTransport(g *transportGeneration) bool {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	if nm.err != nil || nm.stopped || nm.active != nil || g.retired || g.ctx.Err() != nil {
		return false
	}
	g.id = nm.heartbeatStats.Generation()
	nm.active = g
	nm.notifyTransportLocked()
	return true
}
func (nm *transportSupervisor) retireTransport(g *transportGeneration) {
	g.retireOnce.Do(func() {
		// Interrupt in-flight watcher RPCs before waiting for their apply ownership.
		g.cancel()
		g.applyMu.Lock()
		nm.mu.Lock()
		g.retired = true
		if nm.active == g {
			nm.active = nil
			nm.heartbeatStats.BeginGeneration()
			nm.notifyTransportLocked()
		}
		conns := make([]*generationConn, 0, len(g.conns))
		for c := range g.conns {
			conns = append(conns, c)
		}
		nm.mu.Unlock()
		g.applyMu.Unlock()
		for _, c := range conns {
			c.Close()
		}
		for _, h := range g.handles {
			h.Stop()
		}
		g.dials.Wait()
	})
}

func (nm *transportSupervisor) dialTransport(ctx context.Context, endpoint transportEndpoint, base endpoint.Transporter) (net.Conn, error) {
	g, err := nm.waitTransport(ctx)
	if err != nil {
		return nil, err
	}
	return nm.dialGeneration(ctx, g, endpoint, base)
}

func (nm *transportSupervisor) dialGeneration(ctx context.Context, g *transportGeneration, endpoint transportEndpoint, base endpoint.Transporter) (net.Conn, error) {
	nm.mu.Lock()
	if g.retired || g.ctx.Err() != nil || nm.active != g {
		nm.mu.Unlock()
		return nil, net.ErrClosed
	}
	g.dials.Add(1)
	nm.mu.Unlock()
	defer g.dials.Done()
	dialCtx, cancel := context.WithTimeout(ctx, rentIPCallTimeout)
	stop := context.AfterFunc(g.ctx, cancel)
	defer func() { stop(); cancel() }()
	addr := g.endpoints.tcp
	switch endpoint {
	case endpointUDP:
		addr = g.endpoints.udp
	case endpointXDS:
		addr = g.endpoints.xds
	}
	conn, err := base.Dial(dialCtx, addr)
	if err == nil {
		// Preserve TCPTransporter's complete TLS policy; force the lazy handshake here
		// so trust/auth failures reach the supervisor rather than an opaque slot read.
		if tlsConn, ok := conn.(*tls.Conn); ok {
			err = tlsConn.HandshakeContext(dialCtx)
		}
	}
	nm.mu.Lock()
	stale := g.retired || nm.active != g
	if err == nil && !stale && dialCtx.Err() == nil {
		c := &generationConn{Conn: conn, nm: nm, generation: g}
		g.conns[c] = struct{}{}
		nm.mu.Unlock()
		return c, nil
	}
	nm.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
	if stale {
		return nil, net.ErrClosed
	}
	if err == nil {
		err = dialCtx.Err()
	}
	if terminalTransportError(err) {
		nm.signalTransportFailure(g.id, err)
	}
	return nil, err
}

func (nm *transportSupervisor) watchGeneration(ctx context.Context) (context.Context, context.CancelFunc, *transportGeneration, error) {
	g, err := nm.waitTransport(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	watchCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(g.ctx, cancel)
	return watchCtx, func() { stop(); cancel() }, g, nil
}
func (nm *transportSupervisor) applyGeneration(ctx context.Context, g *transportGeneration, apply func() error) error {
	g.applyMu.Lock()
	defer g.applyMu.Unlock()
	nm.mu.Lock()
	active := nm.active == g && !g.retired && !nm.stopped && g.ctx.Err() == nil && ctx.Err() == nil
	nm.mu.Unlock()
	if !active {
		return net.ErrClosed
	}
	return apply()
}
func (nm *transportSupervisor) watchXDSOptions(g *transportGeneration) []grpc.DialOption {
	return []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return nm.dialGeneration(ctx, g, endpointXDS, plainTransportDialer{})
	})}
}

func (nm *transportSupervisor) xdsOptions() []grpc.DialOption {
	return []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return nm.dialTransport(ctx, endpointXDS, plainTransportDialer{})
	})}
}

type plainTransportDialer struct{}

func (plainTransportDialer) Dial(ctx context.Context, addr string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
}

const managerTarget = "passthrough:///kubevpn-manager"

// Failure signals retain their originating generation. Runtime watch errors do
// not call this: only bootstrap and terminal transport errors affect recovery.
func (nm *transportSupervisor) signalTransportFailure(id uint64, err error) {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	if nm.stopped || nm.active == nil || nm.active.id != id {
		return
	}
	if terminalTransportError(err) {
		nm.err = err
		nm.notifyTransportLocked()
	}
	// Replace an older coalesced event under the publication lock so an old
	// generation cannot fill the queue and hide the current bootstrap failure.
	select {
	case <-nm.failures:
	default:
	}
	nm.failures <- transportEvent{id, err}
}
func (nm *transportSupervisor) requestNetworkCheck() {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	if nm.stopped || nm.active == nil {
		return
	}
	select {
	case <-nm.checks:
	default:
	}
	nm.checks <- nm.active.id
}
func (nm *transportSupervisor) armTransportHealth() {
	nm.mu.Lock()
	nm.armed = true
	nm.mu.Unlock()
	nm.requestNetworkCheck()
}
func terminalTransportError(err error) bool {
	if err == nil {
		return false
	}
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verification *tls.CertificateVerificationError
	var channel *gossh.OpenChannelError
	var apiStatus apierrors.APIStatus
	if errors.As(err, &apiStatus) && (apiStatus.Status().Code == 401 || apiStatus.Status().Code == 403) {
		return true
	}
	if errors.Is(err, config.ErrSSHAuth) || errors.Is(err, config.ErrSSHConfig) || errors.Is(err, config.ErrGSSAPI) || errors.As(err, &unknown) || errors.As(err, &hostname) || errors.As(err, &invalid) || errors.As(err, &verification) {
		return true
	}
	if errors.As(err, &channel) && channel.Reason == gossh.Prohibited {
		return true
	}
	switch status.Code(err) {
	case codes.Unauthenticated, codes.PermissionDenied, codes.InvalidArgument:
		return true
	}
	// These errors originate in TLS/key/config parsers that expose no typed error.
	text := strings.ToLower(err.Error())
	for _, s := range []string{"tls config invalid", "tls: bad certificate", "tls: certificate required", "tls: unknown certificate authority", "knownhosts:", "ssh: handshake failed: host key", "invalid transport configuration"} {
		if strings.Contains(text, s) {
			return true
		}
	}
	return false
}

func (nm *transportSupervisor) prepareTransport(ctx context.Context, kind transportKind) (g *transportGeneration, err error) {
	gctx, cancel := context.WithCancel(ctx)
	g = &transportGeneration{kind: kind, stage: "configuration", ctx: gctx, cancel: cancel, conns: make(map[*generationConn]struct{})}
	defer func() {
		if err != nil {
			cancel()
			for _, h := range g.handles {
				h.Stop()
			}
		}
	}()
	ports := []int{config.PortTCP, config.PortUDP, config.PortXDS}
	addresses := make([]string, 0, 3)
	if kind == transportSSH {
		if nm.cfg.SshConf == nil || nm.cfg.SshConf.IsEmpty() || nm.cfg.Clientset == nil {
			return g, fmt.Errorf("invalid transport configuration: SSH requires config and client")
		}
		probeCtx, probeCancel := context.WithTimeout(gctx, 3*time.Second)
		defer probeCancel()
		g.stage = "service_lookup"
		svc, e := nm.cfg.Clientset.CoreV1().Services(nm.cfg.ManagerNamespace).Get(probeCtx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if e != nil {
			return g, e
		}
		ip, e := netip.ParseAddr(svc.Spec.ClusterIP)
		if e != nil {
			return g, fmt.Errorf("SSH service has no reachable ClusterIP: %w", e)
		}
		g.stage = "ssh_handshake"
		client, e := ssh.DialSshRemote(probeCtx, nm.cfg.SshConf, probeCtx.Done())
		if e != nil {
			return g, e
		}
		// PortMap's listeners are lazy. Check actual SSH channels before publication,
		// without sending any fake VPN packets to the manager.
		g.stage = "ssh_channel"
		for _, port := range ports {
			c, e := client.DialContext(probeCtx, "tcp", netip.AddrPortFrom(ip, uint16(port)).String())
			if e != nil {
				client.Close()
				return g, e
			}
			c.Close()
		}
		client.Close()
		g.stage = "ssh_portmap"
		for _, port := range ports {
			h, e := ssh.StartPortMap(gctx, nm.cfg.SshConf, netip.AddrPortFrom(ip, uint16(port)), netip.MustParseAddrPort("127.0.0.1:0"))
			if e != nil {
				return g, e
			}
			g.handles = append(g.handles, h)
			addresses = append(addresses, h.Addr().String())
		}
	} else {
		if nm.cfg.Config == nil || nm.cfg.RESTClient == nil || nm.cfg.GetRunningPodList == nil {
			return g, fmt.Errorf("invalid transport configuration: port-forward requires Kubernetes client")
		}
		g.stage = "pod_list"
		listCtx, listCancel := context.WithTimeout(gctx, portForwardPodListTimeout)
		pods, e := nm.cfg.GetRunningPodList(listCtx)
		listCancel()
		if e != nil {
			return g, e
		}
		if len(pods) == 0 {
			return g, fmt.Errorf("no running traffic manager pod")
		}
		_ = nm.addRoute(util.GetPodIP(pods[0])...)
		g.stage = "port_forward"
		h, e := portforward.StartPortForwardSession(gctx, nm.cfg.Config, nm.cfg.RESTClient, pods[0].Name, nm.cfg.ManagerNamespace, ports)
		if e != nil {
			return g, e
		}
		g.handles = append(g.handles, h)
		for _, port := range ports {
			addresses = append(addresses, h.Ports()[port])
		}
	}
	g.stage = "endpoints"
	for _, addr := range addresses {
		if addr == "" {
			return g, fmt.Errorf("transport endpoint missing")
		}
	}
	g.endpoints = transportEndpoints{addresses[0], addresses[1], addresses[2]}
	return g, nil
}

func (nm *transportSupervisor) terminalCause() error {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	return nm.err
}
func (nm *transportSupervisor) waitTransportHealthy(ctx context.Context) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		nm.mu.Lock()
		err := nm.err
		g := nm.active
		ready := g != nil && !g.retired && g.ctx.Err() == nil && nm.heartbeatStats.Snapshot().Generation == g.id && !nm.heartbeatStats.LastReply().IsZero()
		nm.mu.Unlock()
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			if err := nm.terminalCause(); err != nil {
				return err
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// transportDiagnostics is owned by the supervisor goroutine, including its
// synchronous monitor. It observes recovery without participating in decisions.
type transportDiagnostics struct {
	warnings *plog.Throttle
	baseline tunnel.HeartbeatSnapshot
	outage   time.Time
	failures uint64 // total prepare/retire failures in the current outage
	// pending counts all supervisor prepare/retire failures since the last
	// emitted failure record, across kinds and stages; recovery also resets it.
	pending uint64
	attempt uint64
}

func transportKindName(kind transportKind) string {
	if kind == transportSSH {
		return "ssh"
	}
	return "port-forward"
}

var errTransportHeartbeatExpired = errors.New("data-plane heartbeat expired")

func transportFailureReason(err error) string {
	if terminalTransportError(err) {
		return "terminal"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "timeout"
	}
	if errors.Is(err, errTransportHeartbeatExpired) {
		return "heartbeat_expired"
	}
	if err == nil {
		return "transport_closed"
	}
	return "transport_error"
}

func transportDiagnosticError(err error) string {
	// Transport errors can embed proxy URLs or credential-plugin output. Keep
	// only typed classifications; truncating arbitrary text does not redact it.
	if err == nil {
		return "closed"
	}
	if errors.Is(err, config.ErrSSHAuth) {
		return "ssh_authentication_failed"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return "connection_closed"
	}
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		return "authorization_denied"
	}
	if code := status.Code(err); code != codes.Unknown {
		return "rpc_" + code.String()
	}
	return transportFailureReason(err)
}

func (d *transportDiagnostics) failed(now time.Time) {
	if d.outage.IsZero() {
		d.outage = now
	}
	d.failures++
	d.pending++
}

func (d *transportDiagnostics) recovered(ctx context.Context, g *transportGeneration, snapshot tunnel.HeartbeatSnapshot) {
	if d == nil || d.outage.IsZero() || snapshot.Generation != g.id || snapshot.LastReply.IsZero() {
		return
	}
	plog.G(ctx).WithFields(map[string]any{
		"generation": g.id, "kind": transportKindName(g.kind),
		"outage": time.Since(d.outage).Round(time.Millisecond), "failures": d.failures,
		"evidence": "current_generation_heartbeat_reply",
	}).Info("transport recovered")
	d.outage = time.Time{}
	d.failures, d.pending = 0, 0
	d.warnings.Reset()
}

func (d *transportDiagnostics) retiring(ctx context.Context, g *transportGeneration, primed bool, err error, snapshot tunnel.HeartbeatSnapshot) {
	d.failed(time.Now())
	if !d.warnings.Allow("retire:" + transportKindName(g.kind)) {
		return
	}
	age := "never"
	if !snapshot.LastReply.IsZero() {
		age = time.Since(snapshot.LastReply).Round(time.Millisecond).String()
	}
	plog.G(ctx).WithFields(map[string]any{
		"generation": g.id, "kind": transportKindName(g.kind), "primed": primed,
		"reason": transportFailureReason(err), "error": transportDiagnosticError(err),
		"heartbeat_age": age, "pending_probes": snapshot.Pending,
		"sent_total": snapshot.Sent, "replies_total": snapshot.Replies, "probe_state": snapshot.ProbeState,
		"sent_delta": snapshot.Sent - d.baseline.Sent, "replies_delta": snapshot.Replies - d.baseline.Replies,
		"data_connected": snapshot.Slots[0].Connected, "control_connected": snapshot.Slots[tunnel.ConnPoolSize].Connected,
		"data_read_packets": snapshot.Slots[0].ReadPackets, "data_write_packets": snapshot.Slots[0].WritePackets,
		"data_last_read": snapshot.Slots[0].LastRead, "data_last_write": snapshot.Slots[0].LastWrite,
		"control_write_packets": snapshot.Slots[tunnel.ConnPoolSize].WritePackets,
		"location":              "undetermined", "supervisor_failures_delta": d.pending,
	}).Warn("transport retiring")
	d.pending = 0
}

func (nm *transportSupervisor) launchTransportSupervisor() {
	nm.startWorker(func() { nm.superviseTransport(nm.ctx) })
}
func (nm *transportSupervisor) superviseTransport(ctx context.Context) {
	kinds := []transportKind{transportPF}
	if nm.cfg.SshConf != nil && !nm.cfg.SshConf.IsEmpty() {
		kinds = []transportKind{transportSSH, transportPF}
	}
	index, failed := 0, 0
	delay := portForwardReconnectDelay
	consecutiveDead := 0
	diagnostic := &transportDiagnostics{warnings: plog.NewThrottle(30 * time.Second)}
	for ctx.Err() == nil {
		if nm.terminalCause() != nil {
			nm.cancel()
			return
		}
		hadSession := false
		diagnostic.attempt++
		prepareStarted := time.Now()
		g, err := nm.prepareTransport(ctx, kinds[index])
		if err == nil {
			diagnostic.baseline = nm.heartbeatStats.Snapshot()
			g.diagnostic = diagnostic
			if !nm.publishTransport(g) {
				nm.retireTransport(g)
				if nm.terminalCause() != nil {
					nm.cancel()
				}
				return
			}
			started := time.Now()
			primed, e := nm.monitorTransport(ctx, g)
			if ctx.Err() == nil {
				diagnostic.retiring(ctx, g, primed, e, nm.heartbeatStats.Snapshot())
			}
			nm.retireTransport(g)
			err = e
			if ctx.Err() != nil {
				return
			}
			delay = nextPortForwardDelay(delay, time.Since(started), primed)
			hadSession = true
			if primed {
				consecutiveDead = 0
			} else {
				consecutiveDead++
			}
			if consecutiveDead >= portForwardBlackHoleThreshold {
				blackHoleWarn.Warnf(ctx, "transport-black-hole", "Data plane has not come up across %d transport generations; retrying with bounded backoff", consecutiveDead)
			}
			if primed {
				failed = 0
			}
		} else if ctx.Err() == nil {
			diagnostic.failed(time.Now())
			if diagnostic.warnings.Allow("prepare:" + transportKindName(g.kind)) {
				plog.G(ctx).WithFields(map[string]any{
					"attempt": diagnostic.attempt, "kind": transportKindName(g.kind), "stage": g.stage,
					"elapsed": time.Since(prepareStarted).Round(time.Millisecond), "reason": transportFailureReason(err),
					"error": transportDiagnosticError(err), "supervisor_failures_delta": diagnostic.pending,
				}).Warn("transport prepare_failed")
				diagnostic.pending = 0
			}
		}
		if ctx.Err() != nil {
			return
		}
		if terminal := nm.terminalCause(); terminal != nil {
			err = terminal
		}
		if terminalTransportError(err) {
			nm.mu.Lock()
			nm.err = err
			nm.notifyTransportLocked()
			nm.mu.Unlock()
			plog.G(ctx).WithFields(map[string]any{
				"kind": transportKindName(g.kind), "reason": "terminal", "error": transportDiagnosticError(err),
			}).Error("transport stopped")
			nm.cancel()
			return
		}
		failed++
		index = (index + 1) % len(kinds)
		// Try the other configured path immediately. Once all paths have failed,
		// apply the existing bounded backoff, including never-primed black holes.
		if failed >= len(kinds) {
			retryCtx := plog.WithFields(ctx, map[string]any{
				"next_kind": transportKindName(kinds[index]), "attempt": diagnostic.attempt,
			})
			if !nm.waitNetworkRetry(retryCtx, delay, diagnostic.warnings) {
				return
			}
			if !hadSession {
				delay = nextPortForwardDelay(delay, 0, true)
			}
			failed = 0
		} else if diagnostic.warnings.Allow("switch:" + transportKindName(kinds[index])) {
			plog.G(ctx).WithFields(map[string]any{
				"next_kind": transportKindName(kinds[index]), "attempt": diagnostic.attempt, "retry_in": time.Duration(0),
			}).Info("transport retry")
		}
	}
}
func (nm *transportSupervisor) monitorTransport(ctx context.Context, g *transportGeneration) (bool, error) {
	ticker := time.NewTicker(livenessCheckInterval)
	defer ticker.Stop()
	var armedAt, probeRequested time.Time
	var probeDeadline <-chan time.Time
	var networkDebounce <-chan time.Time
	primed := false
	requestFreshProbe := func() {
		// Repeated notifications cannot extend the recovery budget forever.
		// Only a new path event clears evidence; ordinary ticks never do.
		if !armedAt.IsZero() && probeRequested.IsZero() {
			probeRequested = time.Now()
			probeDeadline = time.After(livenessSteadyThreshold)
		}
		nm.heartbeatStats.ClearPending()
		nm.heartbeatStats.RequestProbe()
	}
	var done [3]<-chan struct{}
	for i, h := range g.handles {
		done[i] = h.Done()
	}
	for {
		if terminal := nm.terminalCause(); terminal != nil {
			return primed, terminal
		}
		for _, h := range g.handles {
			select {
			case <-h.Done():
				return primed, h.Err()
			default:
			}
		}
		nm.mu.Lock()
		armed, terminal := nm.armed, nm.err
		nm.mu.Unlock()
		if terminal != nil {
			return primed, terminal
		}
		if armed && armedAt.IsZero() {
			armedAt = time.Now()
			nm.heartbeatStats.RequestProbe()
		}
		if !armedAt.IsZero() {
			last := nm.heartbeatStats.LastReply()
			if nm.lastReply != nil {
				last = nm.lastReply()
			}
			if last.After(armedAt) {
				if !primed && ctx.Err() == nil {
					g.diagnostic.recovered(ctx, g, nm.heartbeatStats.Snapshot())
				}
				primed = true
			}
			if !probeRequested.IsZero() && last.After(probeRequested) {
				probeRequested = time.Time{}
				probeDeadline = nil
			}
			probeExpired := !probeRequested.IsZero() && time.Since(probeRequested) >= livenessSteadyThreshold
			if probeExpired || (probeRequested.IsZero() && ((!primed && time.Since(armedAt) >= livenessStartupDeadline) || (primed && time.Since(last) >= livenessSteadyThreshold))) {
				// A queued resume/path change gets a fresh-evidence budget before
				// pre-sleep health can retire this generation. An already-expired
				// probe budget remains bounded even during notification storms.
				if !probeExpired && nm.consumeNetworkChange() {
					requestFreshProbe()
					continue
				}
				if runtime.GOOS == "darwin" {
					nm.repairRoutes(ctx)
				}
				return primed, errTransportHeartbeatExpired
			}
		}
		select {
		case <-ctx.Done():
			return primed, ctx.Err()
		case <-ticker.C:
			if nm.consumeNetworkChange() {
				requestFreshProbe()
			}
		case <-nm.monitorChanges:
			if networkDebounce == nil {
				networkDebounce = time.After(50 * time.Millisecond)
			}
		case <-networkDebounce:
			networkDebounce = nil
			if nm.consumeNetworkChange() {
				requestFreshProbe()
			}
		case <-probeDeadline:
		case <-done[0]:
		case <-done[1]:
		case <-done[2]:
		case id := <-nm.checks:
			if id == g.id {
				nm.heartbeatStats.RequestProbe()
				if primed && probeRequested.IsZero() {
					probeRequested = time.Now()
					probeDeadline = time.After(livenessSteadyThreshold)
				}
			}
		case event := <-nm.failures:
			if event.generation == g.id {
				return primed, event.err
			}
		}
	}
}

func (nm *transportSupervisor) startWatchWorker(ctx context.Context, fn func(context.Context)) {
	nm.startWorker(func() {
		watchCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		if nm.ctx != nil {
			stop := context.AfterFunc(nm.ctx, cancel)
			defer stop()
		}
		fn(watchCtx)
	})
}

// Add and Wait are serialized by mu, so Stop also rejects future
// watchers/hooks instead of racing a zero-count WaitGroup.
func (nm *transportSupervisor) startWorker(fn func()) bool {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	if nm.stopped {
		return false
	}
	nm.workers.Add(1)
	go func() { defer nm.workers.Done(); fn() }()
	return true
}

// begin registers startup ownership under the same lock as worker admission.
func (nm *transportSupervisor) begin(ctx context.Context) error {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	if nm.stopped {
		return net.ErrClosed
	}
	nm.ctx, nm.cancel = context.WithCancel(ctx)
	nm.startupDone = make(chan struct{})
	return nil
}

// finishStartup releases startup ownership before Runtime can call Stop on error.
func (nm *transportSupervisor) finishStartup(err error) error {
	if terminal := nm.terminalCause(); terminal != nil {
		err = terminal
	}
	close(nm.startupDone)
	return err
}

// stop closes admission, cancels work, retires sockets/handles, then joins workers.
// Runtime serializes calls; owned workers only cancel, never join themselves.
func (nm *transportSupervisor) stop() {
	nm.mu.Lock()
	nm.stopped = true
	g := nm.active
	startupDone := nm.startupDone
	if nm.changed != nil {
		nm.notifyTransportLocked()
	}
	nm.mu.Unlock()
	if nm.cancel != nil {
		nm.cancel()
	}
	if g != nil {
		nm.retireTransport(g)
	}
	if startupDone != nil {
		<-startupDone
	}
	nm.workers.Wait()
}
