package handler

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
	gossh "golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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

// All publication, retirement and returned-connection registration use transportMu.
// No network operation or blocking Close/Stop runs while that mutex is held.
type transportGeneration struct {
	id         uint64
	kind       transportKind
	ctx        context.Context
	cancel     context.CancelFunc
	endpoints  transportEndpoints
	handles    []transportHandle
	retired    bool
	retireOnce sync.Once
	conns      map[*generationConn]struct{}
	dials      sync.WaitGroup
	applyMu    sync.Mutex // serializes watcher effects with retirement, never transportMu
}
type transportEvent struct {
	generation uint64
	err        error
}
type generationConn struct {
	net.Conn
	nm         *NetworkManager
	generation *transportGeneration
	once       sync.Once
	err        error
}

func (c *generationConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.nm.transportMu.Lock()
		delete(c.generation.conns, c)
		c.nm.transportMu.Unlock()
	})
	return c.err
}

type generationTransporter struct {
	nm       *NetworkManager
	base     core.Transporter
	endpoint transportEndpoint
}

func (t *generationTransporter) Dial(ctx context.Context, _ string) (net.Conn, error) {
	return t.nm.dialTransport(ctx, t.endpoint, t.base)
}

func (nm *NetworkManager) waitTransport(ctx context.Context) (*transportGeneration, error) {
	for {
		nm.transportMu.Lock()
		g, changed, terminal, stopped := nm.activeTransport, nm.transportChanged, nm.transportErr, nm.transportStopped
		nm.transportMu.Unlock()
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
func (nm *NetworkManager) notifyTransportLocked() {
	close(nm.transportChanged)
	nm.transportChanged = make(chan struct{})
}
func (nm *NetworkManager) publishTransport(g *transportGeneration) bool {
	nm.transportMu.Lock()
	defer nm.transportMu.Unlock()
	if nm.transportErr != nil || nm.transportStopped || nm.activeTransport != nil || g.retired || g.ctx.Err() != nil {
		return false
	}
	g.id = nm.heartbeatStats.Generation()
	nm.activeTransport = g
	nm.notifyTransportLocked()
	return true
}
func (nm *NetworkManager) retireTransport(g *transportGeneration) {
	g.retireOnce.Do(func() {
		// Interrupt in-flight watcher RPCs before waiting for their apply ownership.
		g.cancel()
		g.applyMu.Lock()
		nm.transportMu.Lock()
		g.retired = true
		if nm.activeTransport == g {
			nm.activeTransport = nil
			nm.heartbeatStats.BeginGeneration()
			nm.notifyTransportLocked()
		}
		conns := make([]*generationConn, 0, len(g.conns))
		for c := range g.conns {
			conns = append(conns, c)
		}
		nm.transportMu.Unlock()
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

func (nm *NetworkManager) dialTransport(ctx context.Context, endpoint transportEndpoint, base core.Transporter) (net.Conn, error) {
	g, err := nm.waitTransport(ctx)
	if err != nil {
		return nil, err
	}
	return nm.dialGeneration(ctx, g, endpoint, base)
}

func (nm *NetworkManager) dialGeneration(ctx context.Context, g *transportGeneration, endpoint transportEndpoint, base core.Transporter) (net.Conn, error) {
	nm.transportMu.Lock()
	if g.retired || g.ctx.Err() != nil || nm.activeTransport != g {
		nm.transportMu.Unlock()
		return nil, net.ErrClosed
	}
	g.dials.Add(1)
	nm.transportMu.Unlock()
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
	nm.transportMu.Lock()
	stale := g.retired || nm.activeTransport != g
	if err == nil && !stale && dialCtx.Err() == nil {
		c := &generationConn{Conn: conn, nm: nm, generation: g}
		g.conns[c] = struct{}{}
		nm.transportMu.Unlock()
		return c, nil
	}
	nm.transportMu.Unlock()
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

func (nm *NetworkManager) watchGeneration(ctx context.Context) (context.Context, context.CancelFunc, *transportGeneration, error) {
	g, err := nm.waitTransport(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	watchCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(g.ctx, cancel)
	return watchCtx, func() { stop(); cancel() }, g, nil
}
func (nm *NetworkManager) applyGeneration(ctx context.Context, g *transportGeneration, apply func() error) error {
	g.applyMu.Lock()
	defer g.applyMu.Unlock()
	nm.transportMu.Lock()
	active := nm.activeTransport == g && !g.retired && !nm.transportStopped && g.ctx.Err() == nil && ctx.Err() == nil
	nm.transportMu.Unlock()
	if !active {
		return net.ErrClosed
	}
	return apply()
}
func (nm *NetworkManager) watchXDSOptions(g *transportGeneration) []grpc.DialOption {
	return []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return nm.dialGeneration(ctx, g, endpointXDS, plainTransportDialer{})
	})}
}

func (nm *NetworkManager) xdsOptions() []grpc.DialOption {
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
func (nm *NetworkManager) signalTransportFailure(id uint64, err error) {
	nm.transportMu.Lock()
	defer nm.transportMu.Unlock()
	if nm.transportStopped || nm.activeTransport == nil || nm.activeTransport.id != id {
		return
	}
	if terminalTransportError(err) {
		nm.transportErr = err
		nm.notifyTransportLocked()
	}
	// Replace an older coalesced event under the publication lock so an old
	// generation cannot fill the queue and hide the current bootstrap failure.
	select {
	case <-nm.transportFailures:
	default:
	}
	nm.transportFailures <- transportEvent{id, err}
}
func (nm *NetworkManager) requestNetworkCheck() {
	nm.transportMu.Lock()
	defer nm.transportMu.Unlock()
	if nm.transportStopped || nm.activeTransport == nil {
		return
	}
	select {
	case <-nm.networkChecks:
	default:
	}
	nm.networkChecks <- nm.activeTransport.id
}
func (nm *NetworkManager) armTransportHealth() {
	nm.transportMu.Lock()
	nm.transportArmed = true
	nm.transportMu.Unlock()
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

func (nm *NetworkManager) prepareTransport(ctx context.Context, kind transportKind) (g *transportGeneration, err error) {
	gctx, cancel := context.WithCancel(ctx)
	g = &transportGeneration{kind: kind, ctx: gctx, cancel: cancel, conns: make(map[*generationConn]struct{})}
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
		svc, e := nm.cfg.Clientset.CoreV1().Services(nm.cfg.ManagerNamespace).Get(probeCtx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if e != nil {
			return g, e
		}
		ip, e := netip.ParseAddr(svc.Spec.ClusterIP)
		if e != nil {
			return g, fmt.Errorf("SSH service has no reachable ClusterIP: %w", e)
		}
		client, e := ssh.DialSshRemote(probeCtx, nm.cfg.SshConf, probeCtx.Done())
		if e != nil {
			return g, e
		}
		// PortMap's listeners are lazy. Check actual SSH channels before publication,
		// without sending any fake VPN packets to the manager.
		for _, port := range ports {
			c, e := client.DialContext(probeCtx, "tcp", netip.AddrPortFrom(ip, uint16(port)).String())
			if e != nil {
				client.Close()
				return g, e
			}
			c.Close()
		}
		client.Close()
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
		listCtx, listCancel := context.WithTimeout(gctx, portForwardPodListTimeout)
		pods, e := nm.cfg.GetRunningPodList(listCtx)
		listCancel()
		if e != nil {
			return g, e
		}
		if len(pods) == 0 {
			return g, fmt.Errorf("no running traffic manager pod")
		}
		_ = nm.AddRoute(util.GetPodIP(pods[0])...)
		h, e := util.StartPortForwardSession(gctx, nm.cfg.Config, nm.cfg.RESTClient, pods[0].Name, nm.cfg.ManagerNamespace, ports)
		if e != nil {
			return g, e
		}
		g.handles = append(g.handles, h)
		for _, port := range ports {
			addresses = append(addresses, h.Ports()[port])
		}
	}
	for _, addr := range addresses {
		if addr == "" {
			return g, fmt.Errorf("transport endpoint missing")
		}
	}
	g.endpoints = transportEndpoints{addresses[0], addresses[1], addresses[2]}
	return g, nil
}

func (nm *NetworkManager) terminalCause() error {
	nm.transportMu.Lock()
	defer nm.transportMu.Unlock()
	return nm.transportErr
}
func (nm *NetworkManager) waitTransportHealthy(ctx context.Context) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		nm.transportMu.Lock()
		err := nm.transportErr
		g := nm.activeTransport
		ready := g != nil && !g.retired && g.ctx.Err() == nil && nm.heartbeatStats.Snapshot().Generation == g.id && !nm.heartbeatStats.LastReply().IsZero()
		nm.transportMu.Unlock()
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

func (nm *NetworkManager) launchTransportSupervisor() {
	nm.startWorker(func() { nm.superviseTransport(nm.ctx) })
}
func (nm *NetworkManager) superviseTransport(ctx context.Context) {
	kinds := []transportKind{transportPF}
	if nm.cfg.SshConf != nil && !nm.cfg.SshConf.IsEmpty() {
		kinds = []transportKind{transportSSH, transportPF}
	}
	index, failed := 0, 0
	delay := portForwardReconnectDelay
	consecutiveDead := 0
	for ctx.Err() == nil {
		if nm.terminalCause() != nil {
			nm.cancel()
			return
		}
		hadSession := false
		g, err := nm.prepareTransport(ctx, kinds[index])
		if err == nil {
			if !nm.publishTransport(g) {
				nm.retireTransport(g)
				if nm.terminalCause() != nil {
					nm.cancel()
				}
				return
			}
			started := time.Now()
			primed, e := nm.monitorTransport(ctx, g)
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
		}
		if terminal := nm.terminalCause(); terminal != nil {
			err = terminal
		}
		if terminalTransportError(err) {
			nm.transportMu.Lock()
			nm.transportErr = err
			nm.notifyTransportLocked()
			nm.transportMu.Unlock()
			plog.G(ctx).Errorf("Transport stopped: %v", err)
			nm.cancel()
			return
		}
		failed++
		index = (index + 1) % len(kinds)
		// Try the other configured path immediately. Once all paths have failed,
		// apply the existing bounded backoff, including never-primed black holes.
		if failed >= len(kinds) {
			if !nm.waitNetworkRetry(ctx, delay) {
				return
			}
			if !hadSession {
				delay = nextPortForwardDelay(delay, 0, true)
			}
			failed = 0
		}
	}
}
func (nm *NetworkManager) monitorTransport(ctx context.Context, g *transportGeneration) (bool, error) {
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
		nm.transportMu.Lock()
		armed, terminal := nm.transportArmed, nm.transportErr
		nm.transportMu.Unlock()
		if terminal != nil {
			return primed, terminal
		}
		if armed && armedAt.IsZero() {
			armedAt = time.Now()
			nm.heartbeatStats.RequestProbe()
		}
		if !armedAt.IsZero() {
			last := nm.heartbeatStats.LastReply()
			if nm.transportLastReply != nil {
				last = nm.transportLastReply()
			}
			if last.After(armedAt) {
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
					nm.repairStaleRouteSrc(ctx)
				}
				return primed, fmt.Errorf("data-plane heartbeat expired")
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
		case id := <-nm.networkChecks:
			if id == g.id {
				nm.heartbeatStats.RequestProbe()
				if primed && probeRequested.IsZero() {
					probeRequested = time.Now()
					probeDeadline = time.After(livenessSteadyThreshold)
				}
			}
		case event := <-nm.transportFailures:
			if event.generation == g.id {
				return primed, event.err
			}
		}
	}
}

func (nm *NetworkManager) startWatchWorker(ctx context.Context, fn func(context.Context)) {
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

// Add and Wait are serialized by transportMu, so Stop also rejects future
// watchers/hooks instead of racing a zero-count WaitGroup.
func (nm *NetworkManager) startWorker(fn func()) bool {
	nm.transportMu.Lock()
	defer nm.transportMu.Unlock()
	if nm.transportStopped {
		return false
	}
	nm.workers.Add(1)
	go func() { defer nm.workers.Done(); fn() }()
	return true
}
