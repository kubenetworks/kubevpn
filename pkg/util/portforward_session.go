package util

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	streamspdy "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	pfconstants "k8s.io/apimachinery/pkg/util/portforward"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

const (
	portForwardHandshakeTimeout = 10 * time.Second
	portForwardStartupTimeout   = 60 * time.Second
)

// PortForwardSession owns one upgrade and all requested loopback listeners.
// Stop joins ForwardPorts; Err is available after Done closes.
type PortForwardSession struct {
	cancel context.CancelFunc
	done   chan struct{}
	ports  map[int]string
	err    error
}

func (s *PortForwardSession) Ports() map[int]string {
	ports := make(map[int]string, len(s.ports))
	for k, v := range s.ports {
		ports[k] = v
	}
	return ports
}
func (s *PortForwardSession) Stop()                 { s.cancel(); <-s.done }
func (s *PortForwardSession) Done() <-chan struct{} { return s.done }
func (s *PortForwardSession) Err() error            { return s.err }

// StartPortForwardSession requests OS-assigned IPv4 loopback ports in a single
// client-go forwarder. Readiness is all-or-nothing, unlike ForwardPorts itself.
func StartPortForwardSession(ctx context.Context, config *rest.Config, client *rest.RESTClient, pod, ns string, remotePorts []int) (*PortForwardSession, error) {
	expected := make(map[int]bool, len(remotePorts))
	pairs := make([]string, 0, len(remotePorts))
	for _, p := range remotePorts {
		if p < 1 || p > 65535 || expected[p] {
			return nil, fmt.Errorf("invalid or duplicate remote port %d", p)
		}
		expected[p] = true
		pairs = append(pairs, "0:"+strconv.Itoa(p))
	}
	if len(pairs) == 0 {
		return nil, fmt.Errorf("no remote ports requested")
	}
	return startPortForwardSession(ctx, config, client, pod, ns, pairs, expected)
}

func startPortForwardSession(ctx context.Context, config *rest.Config, client *rest.RESTClient, pod, ns string, pairs []string, expected map[int]bool) (*PortForwardSession, error) {
	ctx, cancel := context.WithCancel(ctx)
	s := &PortForwardSession{cancel: cancel, done: make(chan struct{})}
	target := client.Post().Resource("pods").Namespace(ns).Name(pod).SubResource("portforward").URL()
	d := &sessionDialer{ctx: ctx, config: rest.CopyConfig(config), url: target}
	ready := make(chan struct{})
	pf, err := portforward.NewOnAddresses(d, []string{"127.0.0.1"}, pairs, ctx.Done(), ready, io.Discard, io.Discard)
	if err != nil {
		cancel()
		return nil, err
	}
	startup := time.NewTimer(portForwardStartupTimeout)
	defer startup.Stop()
	started := make(chan error, 1)
	go func() {
		defer close(s.done)
		defer d.close()
		defer cancel()
		result := make(chan error, 1)
		go func() { result <- pf.ForwardPorts() }()
		select {
		case <-ready:
			ports, e := pf.GetPorts()
			if e == nil {
				s.ports, e = validateForwardedPorts(ports, expected)
			}
			if e != nil {
				s.err = e
				cancel()
				d.close()
				<-result
				started <- e
				return
			}
			started <- nil
		case e := <-result:
			if d.err != nil {
				e = d.err
			}
			if ctx.Err() != nil {
				e = ctx.Err()
			}
			if e == nil {
				e = fmt.Errorf("port-forward stopped before readiness")
			}
			s.err = e
			started <- e
			return
		case <-ctx.Done():
			d.close()
			<-result
			s.err = ctx.Err()
			started <- s.err
			return
		}
		select {
		case e := <-result:
			if ctx.Err() == nil {
				s.err = e
			}
		case <-ctx.Done():
			d.close()
			<-result
		}
	}()
	select {
	case err = <-started:
		if err != nil {
			s.Stop()
			return nil, err
		}
		if ctx.Err() != nil {
			s.Stop()
			if s.err != nil {
				return nil, s.err
			}
			return nil, ctx.Err()
		}
		return s, nil
	case <-startup.C:
		s.Stop()
		return nil, fmt.Errorf("port-forward startup: %w", context.DeadlineExceeded)
	case <-ctx.Done():
		s.Stop()
		// The worker also cancels ctx on startup failure. Preserve that failure
		// rather than randomly replacing a StatusError with context.Canceled.
		if s.err != nil {
			return nil, s.err
		}
		return nil, ctx.Err()
	}
}

func validateForwardedPorts(ports []portforward.ForwardedPort, expected map[int]bool) (map[int]string, error) {
	result := make(map[int]string, len(ports))
	for _, p := range ports {
		r := int(p.Remote)
		if p.Local == 0 || !expected[r] || result[r] != "" {
			return nil, fmt.Errorf("incomplete port-forward readiness: local %d remote %d", p.Local, p.Remote)
		}
		result[r] = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(p.Local)))
	}
	if len(result) != len(expected) {
		return nil, fmt.Errorf("incomplete port-forward readiness: got %d ports, expected %d", len(result), len(expected))
	}
	return result, nil
}

// upgradeOwner watches the actual selected socket, including a handshake stuck
// reading response headers. Its timer is stopped on upgrade, not left attached
// to the lifetime of an established tunnel.
type upgradeOwner struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	done   chan struct{}
	timer  *time.Timer
}

func newUpgradeOwner(ctx context.Context) *upgradeOwner {
	ctx, cancel := context.WithCancelCause(ctx)
	o := &upgradeOwner{ctx: ctx, cancel: cancel, conns: make(map[net.Conn]struct{}), done: make(chan struct{})}
	o.timer = time.AfterFunc(portForwardHandshakeTimeout, func() { cancel(context.DeadlineExceeded) })
	go func() {
		defer close(o.done)
		<-ctx.Done()
		o.mu.Lock()
		conns := o.conns
		o.conns = nil
		o.mu.Unlock()
		for c := range conns {
			_ = c.Close()
		}
	}()
	return o
}
func (o *upgradeOwner) set(c net.Conn) {
	o.mu.Lock()
	if o.ctx.Err() != nil {
		o.mu.Unlock()
		_ = c.Close()
		return
	}
	o.conns[c] = struct{}{}
	o.mu.Unlock()
}
func (o *upgradeOwner) close() { o.timer.Stop(); o.cancel(context.Canceled); <-o.done }

// net/http may detach connection setup from the request context. Merge its
// dial context with the owner and register raw sockets before TLS or CONNECT.
func (o *upgradeOwner) dial(dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	if dial == nil {
		dial = (&net.Dialer{Timeout: portForwardHandshakeTimeout}).DialContext
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if o.ctx.Err() != nil {
			return nil, context.Cause(o.ctx)
		}
		ctx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(o.ctx, cancel)
		defer func() { stop(); cancel() }()
		c, err := dial(ctx, network, addr)
		if err != nil && c != nil {
			_ = c.Close()
			c = nil
		}
		if c != nil {
			o.set(c)
		}
		if o.ctx.Err() != nil {
			return nil, context.Cause(o.ctx)
		}
		return c, err
	}
}

type sessionDialer struct {
	ctx    context.Context
	config *rest.Config
	url    *url.URL
	mu     sync.Mutex
	owners []*upgradeOwner
	closed bool
	err    error // read only after ForwardPorts joins (preserve client-go's formatted error)
}

func (d *sessionDialer) close() {
	d.mu.Lock()
	d.closed = true
	owners := append([]*upgradeOwner(nil), d.owners...)
	d.mu.Unlock()
	for _, o := range owners {
		o.close()
	}
}
func (d *sessionDialer) attempt() *upgradeOwner {
	o := newUpgradeOwner(d.ctx)
	d.mu.Lock()
	d.owners = append(d.owners, o)
	closed := d.closed
	d.mu.Unlock()
	if closed {
		o.close()
	}
	return o
}
func (d *sessionDialer) Dial(protocols ...string) (conn httpstream.Connection, protocol string, err error) {
	defer func() { d.err = err }()
	if cmdutil.RemoteCommandWebsockets.IsEnabled() {
		conn, protocol, err = d.dialWebSocket(protocols)
		if err == nil || !httpstream.IsUpgradeFailure(err) {
			return
		}
	}
	return d.dialHTTP(protocols)
}

// upgradedBodyConn keeps net/http's buffered 101 body as the stream. Reading the
// raw GotConn socket would discard any SPDY bytes prefetched with the headers.
type upgradedBodyConn struct {
	net.Conn
	body io.ReadWriteCloser
}

func (c *upgradedBodyConn) Read(b []byte) (int, error)  { return c.body.Read(b) }
func (c *upgradedBodyConn) Write(b []byte) (int, error) { return c.body.Write(b) }
func (c *upgradedBodyConn) Close() error                { return c.body.Close() }

func (d *sessionDialer) dialHTTP(protocols []string) (conn httpstream.Connection, protocol string, err error) {
	o := d.attempt()
	defer func() {
		o.timer.Stop()
		if err != nil {
			if o.ctx.Err() != nil {
				err = context.Cause(o.ctx)
			}
			o.close()
		}
	}()
	cfg := rest.CopyConfig(d.config)
	cfg.NextProtos = []string{"http/1.1"}
	cfg.Dial = o.dial(cfg.Dial)
	// A non-nil per-session proxy also prevents caching a transport whose
	// Dial closure owns this session. Never mutate a cached shared transport.
	if cfg.Proxy == nil {
		cfg.Proxy = utilnet.NewProxierWithNoProxyCIDR(http.ProxyFromEnvironment)
	}
	rt, err := rest.TransportFor(cfg)
	if err != nil {
		return nil, "", err
	}
	defer utilnet.CloseIdleConnectionsFor(rt)
	var selected net.Conn
	ctx := httptrace.WithClientTrace(o.ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { selected = info.Conn; o.set(info.Conn) }})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url.String(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "SPDY/3.1")
	req.Header[httpstream.HeaderProtocolVersion] = protocols
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		return nil, "", upgradeResponseError(resp)
	}
	protocol = resp.Header.Get(httpstream.HeaderProtocolVersion)
	if !headerToken(resp.Header, "Connection", "Upgrade") || !headerToken(resp.Header, "Upgrade", "SPDY/3.1") || !hasProtocol(protocols, protocol) {
		resp.Body.Close()
		return nil, "", fmt.Errorf("invalid SPDY upgrade response or protocol %q", protocol)
	}
	body, ok := resp.Body.(io.ReadWriteCloser)
	if !ok || selected == nil {
		resp.Body.Close()
		return nil, "", fmt.Errorf("SPDY upgrade response is not bidirectional")
	}
	conn, err = streamspdy.NewClientConnectionWithPings(&upgradedBodyConn{Conn: selected, body: body}, portforward.PingPeriod)
	return
}
func headerToken(h http.Header, key, token string) bool {
	for _, v := range h.Values(key) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}
func hasProtocol(protocols []string, p string) bool {
	for _, v := range protocols {
		if v == p {
			return true
		}
	}
	return false
}

// UpgradeFailureError in this client-go version does not unwrap Cause. Expose
// both classifications so callers can inspect the original Kubernetes Status.
type upgradeResponseFailure struct {
	*httpstream.UpgradeFailureError
}

func (e *upgradeResponseFailure) Unwrap() []error { return []error{e.UpgradeFailureError, e.Cause} }

// Preserve API Status while allowing explicit protocol failures to fall back.
// Authentication failures never fall back. Bound reads from misbehaving servers.
func upgradeResponseError(resp *http.Response) error {
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return fmt.Errorf("read upgrade response: %w", err)
	}
	var cause error = fmt.Errorf("upgrade response %s: %s", resp.Status, strings.TrimSpace(string(data)))
	status := metav1.Status{}
	if json.Unmarshal(data, &status) == nil && (status.Kind == "Status" || status.Status == "Failure") {
		if status.Code == 0 {
			status.Code = int32(resp.StatusCode)
		}
		cause = &apierrors.StatusError{ErrStatus: status}
	} else if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		status = metav1.Status{Code: int32(resp.StatusCode), Message: string(data), Reason: metav1.StatusReasonUnauthorized}
		if resp.StatusCode == http.StatusForbidden {
			status.Reason = metav1.StatusReasonForbidden
		}
		cause = &apierrors.StatusError{ErrStatus: status}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || apierrors.IsUnauthorized(cause) || apierrors.IsForbidden(cause) {
		return cause
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusUpgradeRequired, http.StatusNotImplemented:
		return &upgradeResponseFailure{UpgradeFailureError: &httpstream.UpgradeFailureError{Cause: cause}}
	}
	return cause
}

type websocketUpgrade struct {
	dialer websocket.Dialer
	conn   *websocket.Conn
}

func (w *websocketUpgrade) RoundTrip(req *http.Request) (*http.Response, error) {
	target := *req.URL
	switch target.Scheme {
	case "http":
		target.Scheme = "ws"
	case "https":
		target.Scheme = "wss"
	default:
		return nil, fmt.Errorf("unsupported WebSocket scheme %q", target.Scheme)
	}
	c, resp, err := w.dialer.DialContext(req.Context(), target.String(), req.Header)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
			return nil, upgradeResponseError(resp)
		}
		return nil, err
	}
	if !hasProtocol(w.dialer.Subprotocols, c.Subprotocol()) {
		c.Close()
		return nil, &httpstream.UpgradeFailureError{Cause: fmt.Errorf("invalid WebSocket protocol %q", c.Subprotocol())}
	}
	w.conn = c
	return resp, nil
}
func (d *sessionDialer) dialWebSocket(protocols []string) (conn httpstream.Connection, protocol string, err error) {
	o := d.attempt()
	defer func() {
		o.timer.Stop()
		if err != nil {
			if o.ctx.Err() != nil {
				err = context.Cause(o.ctx)
			}
			o.close()
		}
	}()
	cfg := rest.CopyConfig(d.config)
	cfg.NextProtos = []string{"http/1.1"}
	tc, err := cfg.TransportConfig()
	if err != nil {
		return nil, "", err
	}
	tlsConfig, err := transport.TLSConfigFor(tc)
	if err != nil {
		return nil, "", err
	}
	proxy := cfg.Proxy
	if proxy == nil {
		proxy = utilnet.NewProxierWithNoProxyCIDR(http.ProxyFromEnvironment)
	}
	ws := &websocketUpgrade{dialer: websocket.Dialer{TLSClientConfig: tlsConfig, Proxy: proxy, HandshakeTimeout: portForwardHandshakeTimeout, ReadBufferSize: 32*1024 + 1024, WriteBufferSize: 32*1024 + 1024}}
	ws.dialer.NetDialContext = o.dial(cfg.Dial)
	for _, p := range protocols {
		ws.dialer.Subprotocols = append(ws.dialer.Subprotocols, pfconstants.WebsocketsSPDYTunnelingPrefix+p)
	}
	rt, err := rest.HTTPWrappersForConfig(cfg, ws)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(o.ctx, http.MethodGet, d.url.String(), nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return nil, "", err
	}
	if resp.Body != nil {
		resp.Body.Close()
	}
	protocol = strings.TrimPrefix(ws.conn.Subprotocol(), pfconstants.WebsocketsSPDYTunnelingPrefix)
	conn, err = streamspdy.NewClientConnectionWithPings(portforward.NewTunnelingConnection("client", ws.conn), portforward.PingPeriod)
	return
}
