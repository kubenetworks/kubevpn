package handler

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
	gossh "golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/httpstream"
	streamspdy "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
)

// Real Kubernetes SPDY stream pairs, optionally forwarded to a real manager.
func transportForwardServer(t *testing.T, targets map[int]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	count := &atomic.Int32{}
	var mu sync.Mutex
	var conns []httpstream.Connection
	var workers sync.WaitGroup
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		if _, err := httpstream.Handshake(r, w, []string{portforward.PortForwardProtocolV1Name}); err != nil {
			return
		}
		var pairsMu sync.Mutex
		pairs := map[string]httpstream.Stream{}
		conn := streamspdy.NewResponseUpgrader().UpgradeResponse(w, r, func(s httpstream.Stream, reply <-chan struct{}) error {
			id := s.Headers().Get("requestID")
			if s.Headers().Get("streamType") == "error" {
				pairsMu.Lock()
				pairs[id] = s
				pairsMu.Unlock()
				return nil
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer s.Close()
				<-reply
				defer func() {
					pairsMu.Lock()
					e := pairs[id]
					delete(pairs, id)
					pairsMu.Unlock()
					if e != nil {
						e.Close()
					}
				}()
				var port int
				_, _ = fmt.Sscan(s.Headers().Get("port"), &port)
				if target := targets[port]; target != "" {
					c, err := net.DialTimeout("tcp", target, time.Second)
					if err != nil {
						return
					}
					defer c.Close()
					done := make(chan struct{})
					go func() { _, _ = io.Copy(c, s); c.Close(); close(done) }()
					_, _ = io.Copy(s, c)
					s.Close()
					<-done
				} else {
					_, _ = io.Copy(s, s)
				}
			}()
			return nil
		})
		if conn != nil {
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}))
	t.Cleanup(func() {
		srv.Close()
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
		workers.Wait()
	})
	return srv, count
}

func transportManager(t *testing.T, host string) *NetworkManager {
	t.Helper()
	t.Setenv("KUBECTL_REMOTE_COMMAND_WEBSOCKETS", "false")
	cfg := &rest.Config{Host: host, APIPath: "/api", ContentConfig: rest.ContentConfig{GroupVersion: &schema.GroupVersion{Version: "v1"}, NegotiatedSerializer: serializer.NewCodecFactory(runtime.NewScheme()).WithoutConversion()}}
	client, err := rest.RESTClientFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	nm := newNetworkManager(NetworkConfig{Config: cfg, RESTClient: client, ManagerNamespace: "test-ns", OwnerID: "transport-owner", GetRunningPodList: func(context.Context) ([]v1.Pod, error) { return []v1.Pod{{}}, nil }})
	nm.ctx, nm.cancel = context.WithCancel(context.Background())
	t.Cleanup(nm.Stop)
	return nm
}

// SSH fixture uses real authentication and direct-tcpip channel negotiation.
func transportSSHServer(t *testing.T, deny bool, reject gossh.RejectionReason, targets ...map[int]string) *ssh.SshConfig {
	t.Helper()
	_, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	signer, e := gossh.NewSignerFromKey(key)
	if e != nil {
		t.Fatal(e)
	}
	cfg := &gossh.ServerConfig{PasswordCallback: func(gossh.ConnMetadata, []byte) (*gossh.Permissions, error) {
		if deny {
			return nil, errors.New("denied")
		}
		return nil, nil
	}}
	cfg.AddHostKey(signer)
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var conns []net.Conn
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				sc, chs, reqs, e := gossh.NewServerConn(c, cfg)
				if e != nil {
					return
				}
				defer sc.Close()
				go gossh.DiscardRequests(reqs)
				for ch := range chs {
					if reject != 0 {
						ch.Reject(reject, "test rejection")
						continue
					}
					s, rs, e := ch.Accept()
					if e != nil {
						return
					}
					wg.Add(1)
					go func(payload []byte) {
						defer wg.Done()
						defer s.Close()
						go gossh.DiscardRequests(rs)
						var destination struct {
							Host       string
							Port       uint32
							Origin     string
							OriginPort uint32
						}
						if err := gossh.Unmarshal(payload, &destination); err != nil {
							t.Error(err)
							return
						}
						if len(targets) > 0 && targets[0][int(destination.Port)] != "" {
							c, err := net.DialTimeout("tcp", targets[0][int(destination.Port)], time.Second)
							if err != nil {
								return
							}
							defer c.Close()
							done := make(chan struct{})
							go func() { io.Copy(c, s); c.Close(); close(done) }()
							io.Copy(s, c)
							s.Close()
							<-done
						} else {
							io.Copy(s, s)
						}
					}(ch.ExtraData())
				}
			}()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return &ssh.SshConfig{Addr: ln.Addr().String(), User: "test", Password: "test"}
}
func configureTransportSSH(nm *NetworkManager, conf *ssh.SshConfig) {
	nm.cfg.SshConf = conf
	nm.cfg.Clientset = fake.NewSimpleClientset(&v1.Service{ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"}, Spec: v1.ServiceSpec{ClusterIP: "127.0.0.1"}})
}
func TestTransportIntegration_SSHAuthDoesNotFallback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		deny   bool
		reject gossh.RejectionReason
	}{{"auth", true, 0}, {"prohibited", false, gossh.Prohibited}} {
		t.Run(tc.name, func(t *testing.T) {
			srv, count := transportForwardServer(t, nil)
			nm := transportManager(t, srv.URL)
			configureTransportSSH(nm, transportSSHServer(t, tc.deny, tc.reject))
			nm.launchTransportSupervisor()
			ctx, cancel := context.WithTimeout(nm.ctx, 5*time.Second)
			defer cancel()
			_, e := nm.waitTransport(ctx)
			if e == nil {
				t.Fatal("terminal error accepted")
			}
			nm.Stop()
			if count.Load() != 0 {
				t.Fatal("authentication failure fell back to PF")
			}
		})
	}
}

func TestTransportReview_RejectTunWorkerClosesListener(t *testing.T) {
	nm := newNetworkManager(NetworkConfig{})
	nm.Stop()
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer ln.Close()
	nm.newTunListener = func(tun.Config) (net.Listener, error) { return ln, nil }
	if e := nm.startTunServer(context.Background(), &core.Node{Addr: "manager"}, "manager", nil, tun.Config{}); !errors.Is(e, net.ErrClosed) {
		t.Fatalf("rejected worker: %v", e)
	}
	if c, e := net.DialTimeout("tcp", ln.Addr().String(), time.Second); e == nil {
		c.Close()
		t.Fatal("rejected listener leaked")
	}
}
func TestTransportReview_StopWaitsStartup(t *testing.T) {
	nm := newNetworkManager(NetworkConfig{})
	nm.ctx, nm.cancel = context.WithCancel(context.Background())
	nm.startupDone = make(chan struct{})
	done := make(chan struct{})
	go func() { nm.Stop(); close(done) }()
	transportAwait(t, nm.ctx.Done())
	select {
	case <-done:
		t.Fatal("cleanup passed active startup")
	default:
	}
	close(nm.startupDone)
	transportAwait(t, done)
	if e := nm.Start(context.Background()); !errors.Is(e, net.ErrClosed) {
		t.Fatalf("Start after Stop: %v", e)
	}
}

func TestTransportReview_GenerationApplyFence(t *testing.T) {
	srv, _ := transportForwardServer(t, nil)
	nm := transportManager(t, srv.URL)
	g, e := nm.prepareTransport(nm.ctx, transportPF)
	if e != nil {
		t.Fatal(e)
	}
	nm.publishTransport(g)
	entered, release, applied := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		nm.applyGeneration(nm.ctx, g, func() error { close(entered); <-release; return nil })
		close(applied)
	}()
	transportAwait(t, entered)
	retired := make(chan struct{})
	go func() { nm.retireTransport(g); close(retired) }()
	transportAwait(t, g.ctx.Done())
	select {
	case <-retired:
		t.Fatal("retirement escaped active apply owner")
	default:
	}
	close(release)
	transportAwait(t, retired)
	transportAwait(t, applied)
	if e := nm.applyGeneration(nm.ctx, g, func() error { t.Error("buffered retired result applied"); return nil }); e == nil {
		t.Fatal("stale apply accepted")
	}
	replacement, e := nm.prepareTransport(nm.ctx, transportPF)
	if e != nil {
		t.Fatal(e)
	}
	nm.publishTransport(replacement)
	if c, e := nm.dialGeneration(nm.ctx, g, endpointXDS, plainTransportDialer{}); e == nil {
		c.Close()
		t.Fatal("old watcher dial migrated to replacement")
	}
}
func TestTransportReview_TerminalWinsHandleEOF(t *testing.T) {
	srv, count := transportForwardServer(t, nil)
	nm := transportManager(t, srv.URL)
	configureTransportSSH(nm, transportSSHServer(t, false, 0))
	g, e := nm.prepareTransport(nm.ctx, transportSSH)
	if e != nil {
		t.Fatal(e)
	}
	nm.publishTransport(g)
	nm.signalTransportFailure(g.id, config.ErrSSHAuth)
	g.handles[0].Stop()
	if _, e := nm.monitorTransport(nm.ctx, g); !errors.Is(e, config.ErrSSHAuth) {
		t.Fatalf("terminal masked by handle EOF: %v", e)
	}
	nm.launchTransportSupervisor()
	transportAwait(t, nm.ctx.Done())
	if count.Load() != 0 {
		t.Fatal("terminal error prepared alternate PF")
	}
	nm.Stop()
}
func TestTransportReview_StartHealthyGate(t *testing.T) {
	nm := newNetworkManager(NetworkConfig{})
	seedTransportEndpoint(t, nm, "127.0.0.1:1")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if e := nm.waitTransportHealthy(ctx); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("unprimed startup accepted: %v", e)
	}
	nm.transportMu.Lock()
	nm.transportErr = config.ErrSSHAuth
	nm.transportMu.Unlock()
	if e := nm.waitTransportHealthy(ctx); !errors.Is(e, config.ErrSSHAuth) {
		t.Fatalf("terminal startup cause masked: %v", e)
	}
}

type boundedWatchServer struct {
	rpc.UnimplementedTunConfigServiceServer
	deadlines chan time.Duration
	watched   atomic.Bool
}

func (s *boundedWatchServer) GetTunIP(ctx context.Context, req *rpc.TunIPRequest) (*rpc.TunIPResponse, error) {
	d, ok := ctx.Deadline()
	if !ok {
		s.deadlines <- 0
	} else {
		s.deadlines <- time.Until(d)
	}
	return nil, status.Error(codes.Unavailable, "retry watcher")
}
func (s *boundedWatchServer) WatchTunIP(_ *rpc.TunIPRequest, _ rpc.TunConfigService_WatchTunIPServer) error {
	s.watched.Store(true)
	return status.Error(codes.Unavailable, "retry watch stream")
}
func TestTransportReview_WatchUnaryDeadline(t *testing.T) {
	service := &boundedWatchServer{deadlines: make(chan time.Duration, 8)}
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	server := grpc.NewServer()
	rpc.RegisterTunConfigServiceServer(server, service)
	go server.Serve(ln)
	defer server.Stop()
	nm := newNetworkManager(NetworkConfig{OwnerID: "watch"})
	seedTransportEndpoint(t, nm, ln.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var version int64
	done := make(chan error, 1)
	go func() { done <- nm.doWatchTunIP(ctx, &version) }()
	select {
	case d := <-service.deadlines:
		if d <= 0 || d > rentIPCallTimeout+time.Second {
			cancel()
			t.Fatalf("unbounded reassert deadline %v", d)
		}
	case <-time.After(time.Second):
		t.Fatal("RPC not reached")
	}
	select {
	case e := <-done:
		if status.Code(e) != codes.Unavailable || status.Convert(e).Message() != "retry watch stream" {
			t.Fatalf("watch retry error: %v", e)
		}
		if !service.watched.Load() {
			t.Fatal("failed reassert did not fall back to WatchTunIP")
		}
	case <-time.After(time.Second):
		t.Fatal("failed watch stream did not end watch iteration")
	}
	conn, e := grpc.DialContext(ctx, managerTarget, nm.xdsOptions()...)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	client := rpc.NewTunConfigServiceClient(conn)
	for _, ip := range []string{"198.18.2.10", "127.0.0.1"} {
		cand := &net.IPNet{IP: net.ParseIP(ip), Mask: net.CIDRMask(32, 32)}
		if e := nm.handleProposal(ctx, client, cand, nil); status.Code(e) != codes.Unavailable {
			t.Fatalf("proposal error not propagated: %v", e)
		}
		if d := <-service.deadlines; d <= 0 || d > rentIPCallTimeout+time.Second {
			t.Fatalf("unbounded confirm/decline: %v", d)
		}
	}
	if e := nm.applyPushedTunIP(ctx, client, &net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(32, 32)}, nil); status.Code(e) != codes.Unavailable {
		t.Fatalf("reject error: %v", e)
	}
	if d := <-service.deadlines; d <= 0 || d > rentIPCallTimeout+time.Second {
		t.Fatalf("unbounded reject: %v", d)
	}
}

type bootstrapTransportServer struct {
	rpc.UnimplementedTunConfigServiceServer
	delegate rpc.TunConfigServiceServer
	calls    atomic.Int32
}

func (s *bootstrapTransportServer) GetTunIP(ctx context.Context, req *rpc.TunIPRequest) (*rpc.TunIPResponse, error) {
	if s.calls.Add(1) == 1 {
		return nil, status.Error(codes.Unavailable, "bootstrap path unavailable")
	}
	return s.delegate.GetTunIP(ctx, req)
}
func TestTransportIntegration_BootstrapRPCFailureRecovers(t *testing.T) {
	env := newNetworkTestEnv(t)
	service := &bootstrapTransportServer{delegate: env.server}
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	server := grpc.NewServer()
	rpc.RegisterTunConfigServiceServer(server, service)
	go server.Serve(ln)
	defer server.Stop()
	srv, count := transportForwardServer(t, map[int]string{config.PortXDS: ln.Addr().String()})
	nm := transportManager(t, srv.URL)
	nm.launchTransportSupervisor()
	ctx, cancel := context.WithTimeout(nm.ctx, 8*time.Second)
	defer cancel()
	if e := nm.rentIP(ctx); e != nil {
		t.Fatal(e)
	}
	if service.calls.Load() < 2 || count.Load() != 2 || nm.localTunIPv4 == nil {
		t.Fatalf("bootstrap did not replace and allocate: calls=%d upgrades=%d", service.calls.Load(), count.Load())
	}
	nm.Stop()
}
func TestTransportIntegration_XDSOnlyFailureKeepsData(t *testing.T) {
	// Echo is deliberately not an HTTP/2 service, while TCP data is healthy.
	srv, count := transportForwardServer(t, nil)
	nm := transportManager(t, srv.URL)
	nm.transportLastReply = time.Now
	nm.launchTransportSupervisor()
	ctx, cancel := context.WithTimeout(nm.ctx, 5*time.Second)
	defer cancel()
	g, e := nm.waitTransport(ctx)
	if e != nil {
		t.Fatal(e)
	}
	nm.armTransportHealth()
	watchCtx, watchCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer watchCancel()
	var version int64
	if e := nm.doWatchNamespaceRoutes(watchCtx, &version); e == nil {
		t.Fatal("expected xDS failure")
	}
	c, e := nm.dialTransport(ctx, endpointTCP, plainTransportDialer{})
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	transportEcho(t, c)
	if g.ctx.Err() != nil || count.Load() != 1 {
		t.Fatal("xDS-only failure retired healthy data")
	}
	nm.StartIPWatcher(context.Background())
	nm.StartRouteWatcher(context.Background())
	nm.Stop()
}
func TestTransportIntegration_TLSHandshakeRejectsUntrustedPeer(t *testing.T) {
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer tlsServer.Close()
	wrong := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer wrong.Close()
	pair := wrong.TLS.Certificates[0]
	key, e := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if e != nil {
		t.Fatal(e)
	}
	secret := map[string][]byte{config.TLSCertKey: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}), config.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), config.TLSServerName: []byte("untrusted.invalid")}
	nm := newNetworkManager(NetworkConfig{})
	seedTransportEndpoint(t, nm, tlsServer.Listener.Addr().String())
	c, e := nm.dialTransport(context.Background(), endpointTCP, core.TCPTransporter(secret))
	if c != nil {
		c.Close()
		t.Fatal("lazy TLS connection escaped without verification")
	}
	var verification *tls.CertificateVerificationError
	if !errors.As(e, &verification) || !terminalTransportError(e) {
		t.Fatalf("lost trust classification: %v", e)
	}
	nm.Stop()
}

func TestTransportIntegration_UnreachableSSHUsesStickyPF(t *testing.T) {
	srv, count := transportForwardServer(t, nil)
	nm := transportManager(t, srv.URL)
	configureTransportSSH(nm, transportSSHServer(t, false, gossh.ConnectionFailed))
	nm.transportLastReply = time.Now
	nm.launchTransportSupervisor()
	ctx, cancel := context.WithTimeout(nm.ctx, 5*time.Second)
	defer cancel()
	g, e := nm.waitTransport(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if g.kind != transportPF {
		t.Fatal("unreachable SSH was published")
	}
	nm.armTransportHealth()
	for range 20 {
		nm.requestNetworkCheck()
	}
	c, e := nm.dialTransport(ctx, endpointTCP, plainTransportDialer{})
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	transportEcho(t, c)
	if count.Load() != 1 || g.ctx.Err() != nil {
		t.Fatal("healthy fallback not sticky")
	}
	nm.Stop()
}
func TestTransportIntegration_TerminalRuntimeFailureCancelsSession(t *testing.T) {
	srv, _ := transportForwardServer(t, nil)
	nm := transportManager(t, srv.URL)
	nm.launchTransportSupervisor()
	ctx, cancel := context.WithTimeout(nm.ctx, 5*time.Second)
	defer cancel()
	g, e := nm.waitTransport(ctx)
	if e != nil {
		t.Fatal(e)
	}
	c, e := nm.dialTransport(ctx, endpointTCP, plainTransportDialer{})
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	transportEcho(t, c)
	nm.signalTransportFailure(g.id, config.ErrSSHAuth)
	transportAwait(t, nm.ctx.Done())
	nm.Stop()
	if _, e := c.Read(make([]byte, 1)); e == nil {
		t.Fatal("terminal failure left a healthy socket")
	}
	if !nm.LastHeartbeat().IsZero() {
		t.Fatal("terminal failure kept stale healthy statistics")
	}
	if nm.startWorker(func() { t.Error("worker started after Stop") }) {
		t.Fatal("Stop accepted a worker")
	}
	nm.requestNetworkCheck()
	nm.Stop()
}

func TestTransportIntegration_PFAuthDoesNotRetry(t *testing.T) {
	for _, code := range []int{401, 403} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","code":%d,"message":"denied"}`, code)
			}))
			defer srv.Close()
			nm := transportManager(t, srv.URL)
			nm.launchTransportSupervisor()
			ctx, cancel := context.WithTimeout(nm.ctx, 5*time.Second)
			defer cancel()
			if _, e := nm.waitTransport(ctx); e == nil {
				t.Fatal("authorization failure ignored")
			}
			nm.Stop()
			if calls.Load() != 1 {
				t.Fatalf("authorization retried %d times", calls.Load())
			}
		})
	}
}

func TestTransportIntegration_SSHAllEndpointsAndStop(t *testing.T) {
	srv, count := transportForwardServer(t, nil)
	nm := transportManager(t, srv.URL)
	configureTransportSSH(nm, transportSSHServer(t, false, 0))
	g, e := nm.prepareTransport(nm.ctx, transportSSH)
	if e != nil {
		t.Fatal(e)
	}
	nm.publishTransport(g)
	for _, ep := range []transportEndpoint{endpointTCP, endpointUDP, endpointXDS} {
		c, e := nm.dialTransport(nm.ctx, ep, plainTransportDialer{})
		if e != nil {
			t.Fatal(e)
		}
		transportEcho(t, c)
		c.Close()
	}
	nm.Stop()
	for _, h := range g.handles {
		transportAwait(t, h.Done())
	}
	if count.Load() != 0 {
		t.Fatal("unexpected PF")
	}
}

func seedTransportEndpoint(t *testing.T, nm *NetworkManager, addr string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	g := &transportGeneration{ctx: ctx, cancel: cancel, endpoints: transportEndpoints{addr, addr, addr}, conns: make(map[*generationConn]struct{})}
	if !nm.publishTransport(g) {
		t.Fatal("seed generation failed")
	}
	t.Cleanup(func() { nm.retireTransport(g) })
}

// Packet-preserving TUN facade bridges a real originator stack to TunHandler.
// LocalAddr resolves the real loopback interface without creating a TUN device.
type transportPacketTUN struct {
	endpoint *channel.Endpoint
	ctx      context.Context
	cancel   context.CancelFunc
}

func (p *transportPacketTUN) Read(b []byte) (int, error) {
	pkt := p.endpoint.ReadContext(p.ctx)
	if pkt == nil {
		return 0, net.ErrClosed
	}
	defer pkt.DecRef()
	v := pkt.ToView()
	defer v.Release()
	return copy(b, v.AsSlice()), nil
}
func (p *transportPacketTUN) Write(b []byte) (int, error) {
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), b...))})
	defer pkt.DecRef()
	p.endpoint.InjectInbound(header.IPv4ProtocolNumber, pkt)
	return len(b), nil
}
func (p *transportPacketTUN) Close() error                     { p.cancel(); return nil }
func (p *transportPacketTUN) LocalAddr() net.Addr              { return &net.IPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (p *transportPacketTUN) RemoteAddr() net.Addr             { return p.LocalAddr() }
func (p *transportPacketTUN) SetDeadline(time.Time) error      { return nil }
func (p *transportPacketTUN) SetReadDeadline(time.Time) error  { return nil }
func (p *transportPacketTUN) SetWriteDeadline(time.Time) error { return nil }

func waitNextTransport(t *testing.T, nm *NetworkManager, id uint64) *transportGeneration {
	t.Helper()
	ctx, cancel := context.WithTimeout(nm.ctx, 5*time.Second)
	defer cancel()
	for {
		g, e := nm.waitTransport(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if g.id != id {
			return g
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}
func TestTransportIntegration_GvisorApplicationSurvivesReplacement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	echo, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer echo.Close()
	echoDone := make(chan struct{})
	go func() {
		defer close(echoDone)
		c, e := echo.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()
	backend, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	backendDone := make(chan struct{})
	go func() {
		Run(ctx, []core.Server{{Listener: backend, Handler: core.GvisorLocalTCPHandler(core.NewRouteHub())}})
		close(backendDone)
	}()
	srv, _ := transportForwardServer(t, map[int]string{config.PortTCP: backend.Addr().String()})
	nm := transportManager(t, srv.URL)
	configureTransportSSH(nm, transportSSHServer(t, false, 0, map[int]string{config.PortTCP: backend.Addr().String()}))
	nm.launchTransportSupervisor()
	g, e := nm.waitTransport(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if g.kind != transportSSH {
		t.Fatal("initial path is not SSH")
	}
	endpoint := channel.New(256, 1500, tcpip.GetRandMacAddr())
	origin := core.NewLocalStack(ctx, endpoint)
	defer origin.Destroy()
	defer endpoint.Close()
	addr := tcpip.AddrFrom4([4]byte{198, 18, 0, 10})
	if e := origin.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: header.IPv4ProtocolNumber, AddressWithPrefix: addr.WithPrefix()}, stack.AddressProperties{}); e != nil {
		t.Fatal(e)
	}
	tunCtx, tunCancel := context.WithCancel(ctx)
	device := &transportPacketTUN{endpoint, tunCtx, tunCancel}
	handler := core.TunHandler(&core.Forwarder{Addr: "manager", Transporter: &generationTransporter{nm, plainTransportDialer{}, endpointTCP}, Connector: core.NewUDPOverTCPConnector()}, core.NewRouteHub(), nm.heartbeatStats)
	tunDone := make(chan struct{})
	go func() { handler.Handle(ctx, device); close(tunDone) }()
	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	defer dialCancel()
	app, e := gonet.DialContextTCP(dialCtx, origin, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{10, 20, 30, 40}), Port: uint16(echo.Addr().(*net.TCPAddr).Port)}, header.IPv4ProtocolNumber)
	if e != nil {
		cancel()
		t.Fatal(e)
	}
	transportEcho(t, app)
	for _, kind := range []transportKind{transportPF, transportSSH} {
		// Stop a real session handle; only the running supervisor may retire,
		// prepare and publish replacements. No direct generation manipulation.
		g.handles[0].Stop()
		next := waitNextTransport(t, nm, g.id)
		if next.kind != kind {
			t.Fatalf("replacement kind=%v want=%v", next.kind, kind)
		}
		if next.id <= g.id {
			t.Fatal("generation did not advance")
		}
		transportEcho(t, app) // exactly the same application net.Conn
		g = next
	}
	// No second sink connection may have been created by reconnecting the app.
	echo.(*net.TCPListener).SetDeadline(time.Now().Add(50 * time.Millisecond))
	if extra, err := echo.Accept(); err == nil {
		extra.Close()
		t.Fatal("application re-dialed sink")
	}
	app.Close()
	nm.Stop()
	cancel()
	device.Close()
	transportAwait(t, tunDone)
	transportAwait(t, backendDone)
	transportAwait(t, echoDone)
}

type heldTransportHandler struct{ entered, release chan struct{} }

func (h heldTransportHandler) Handle(ctx context.Context, c net.Conn) {
	close(h.entered)
	<-ctx.Done()
	<-h.release
}
func TestTransportIntegration_RunJoinsHandler(t *testing.T) {
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	h := heldTransportHandler{make(chan struct{}), make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { Run(ctx, []core.Server{{Listener: ln, Handler: h}}); close(done) }()
	c, e := net.Dial("tcp", ln.Addr().String())
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	transportAwait(t, h.entered)
	cancel()
	select {
	case <-done:
		close(h.release)
		t.Fatal("Run returned before handler completion")
	case <-time.After(50 * time.Millisecond):
	}
	close(h.release)
	transportAwait(t, done)
}

func transportAwait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not join")
	}
}
func transportEcho(t *testing.T, c net.Conn) {
	t.Helper()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte("echo")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "echo" {
		t.Fatalf("echo %q: %v", b, err)
	}
}

func TestTransportIntegration_GenerationRetiresAllEndpoints(t *testing.T) {
	srv, count := transportForwardServer(t, nil)
	nm := transportManager(t, srv.URL)
	g, err := nm.prepareTransport(nm.ctx, transportPF)
	if err != nil {
		t.Fatal(err)
	}
	if !nm.publishTransport(g) {
		t.Fatal("publish failed")
	}
	var active []net.Conn
	for _, endpoint := range []transportEndpoint{endpointTCP, endpointUDP, endpointXDS} {
		c, e := nm.dialTransport(nm.ctx, endpoint, core.TCPTransporter(nil))
		if e != nil {
			t.Fatal(e)
		}
		active = append(active, c)
		transportEcho(t, c)
	}
	if count.Load() != 1 {
		t.Fatalf("upgrades=%d", count.Load())
	}
	nm.retireTransport(g)
	for _, c := range active {
		if _, e := c.Read(make([]byte, 1)); e == nil {
			t.Fatal("retired socket survived")
		}
		c.Close()
	}
	for _, addr := range []string{g.endpoints.tcp, g.endpoints.udp, g.endpoints.xds} {
		if c, e := net.DialTimeout("tcp", addr, time.Second); e == nil {
			c.Close()
			t.Fatal("retired listener survived")
		}
	}
	nm.Stop()
	nm.Stop()
}

type delayedTransportDial struct {
	entered chan struct{}
	release chan struct{}
	peer    chan net.Conn
}

func (d *delayedTransportDial) Dial(ctx context.Context, addr string) (net.Conn, error) {
	c, e := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if e != nil {
		return nil, e
	}
	d.peer <- c
	close(d.entered)
	<-d.release
	return c, nil
}
func TestTransportIntegration_LateDialNeverPublished(t *testing.T) {
	srv, _ := transportForwardServer(t, nil)
	nm := transportManager(t, srv.URL)
	g, e := nm.prepareTransport(nm.ctx, transportPF)
	if e != nil {
		t.Fatal(e)
	}
	nm.publishTransport(g)
	d := &delayedTransportDial{make(chan struct{}), make(chan struct{}), make(chan net.Conn, 1)}
	result := make(chan error, 1)
	go func() {
		c, e := nm.dialTransport(nm.ctx, endpointTCP, d)
		if c != nil {
			c.Close()
			result <- errors.New("stale successful dial returned")
			return
		}
		result <- e
	}()
	transportAwait(t, d.entered)
	retired := make(chan struct{})
	go func() { nm.retireTransport(g); close(retired) }()
	transportAwait(t, g.ctx.Done())
	close(d.release)
	transportAwait(t, retired)
	if e := <-result; !errors.Is(e, net.ErrClosed) {
		t.Fatalf("late dial: %v", e)
	}
	c := <-d.peer
	if _, e = c.Write([]byte("late")); e == nil {
		t.Fatal("late dial socket not closed")
	}
}

func TestTransportIntegration_SupervisorBootstrapRecoverySticky(t *testing.T) {
	env := newNetworkTestEnv(t)
	srv, count := transportForwardServer(t, map[int]string{config.PortXDS: fmt.Sprintf("127.0.0.1:%d", env.port)})
	nm := transportManager(t, srv.URL)
	nm.launchTransportSupervisor()
	ctx, cancel := context.WithTimeout(nm.ctx, 5*time.Second)
	defer cancel()
	g, e := nm.waitTransport(ctx)
	if e != nil {
		t.Fatal(e)
	}
	nm.signalTransportFailure(g.id, context.DeadlineExceeded)
	for {
		next, e := nm.waitTransport(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if next.id != g.id {
			break
		}
		select {
		case <-time.After(time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if e := nm.rentIP(ctx); e != nil {
		t.Fatal(e)
	}
	if nm.localTunIPv4 == nil {
		t.Fatal("real DHCP allocation missing")
	}
	current, _ := nm.waitTransport(ctx)
	for range 10 {
		nm.requestNetworkCheck()
	}
	c, e := nm.dialTransport(ctx, endpointTCP, core.TCPTransporter(nil))
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	transportEcho(t, c)
	// A stale bootstrap failure must not retire the replacement.
	nm.signalTransportFailure(g.id, context.DeadlineExceeded)
	select {
	case <-current.ctx.Done():
		t.Fatal("stale event retired current generation")
	case <-time.After(100 * time.Millisecond):
	}
	if count.Load() != 2 {
		t.Fatalf("healthy PF was not sticky: %d upgrades", count.Load())
	}
	nm.Stop()
	if _, e = c.Read(make([]byte, 1)); e == nil {
		t.Fatal("Stop left data socket alive")
	}
}
