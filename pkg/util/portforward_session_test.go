package util

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/httpstream"
	streamspdy "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	pfconstants "k8s.io/apimachinery/pkg/util/portforward"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
)

func sessionConfig(t *testing.T, host string) (*rest.Config, *rest.RESTClient) {
	t.Helper()
	cfg := &rest.Config{Host: host, APIPath: "/api", ContentConfig: rest.ContentConfig{GroupVersion: &schema.GroupVersion{Version: "v1"}, NegotiatedSerializer: serializer.NewCodecFactory(runtime.NewScheme()).WithoutConversion()}}
	client, err := rest.RESTClientFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, client
}

// echoForwardServer uses Kubernetes' SPDY implementation and real error/data
// stream pairs, also over the standard WebSocket tunneling adapter.
func echoForwardServer(t *testing.T, ws bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	count := &atomic.Int32{}
	var mu sync.Mutex
	var conns []httpstream.Connection
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		var pairMu sync.Mutex
		pairs := map[string]httpstream.Stream{}
		handler := func(s httpstream.Stream, reply <-chan struct{}) error {
			id := s.Headers().Get("requestID")
			if s.Headers().Get("streamType") == "error" {
				pairMu.Lock()
				pairs[id] = s
				pairMu.Unlock()
				return nil
			}
			go func() {
				<-reply
				_, _ = io.Copy(s, s)
				_ = s.Close()
				pairMu.Lock()
				e := pairs[id]
				delete(pairs, id)
				pairMu.Unlock()
				if e != nil {
					_ = e.Close()
				}
			}()
			return nil
		}
		var conn httpstream.Connection
		if ws && r.Method == http.MethodGet {
			u := websocket.Upgrader{Subprotocols: []string{pfconstants.WebsocketsSPDYTunnelingPortForwardV1}}
			c, err := u.Upgrade(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			conn, err = streamspdy.NewServerConnection(portforward.NewTunnelingConnection("server", c), handler)
			if err != nil {
				t.Error(err)
				return
			}
		} else if r.Method == http.MethodGet {
			http.Error(w, "websocket unsupported", http.StatusBadRequest)
			return
		} else {
			if _, err := httpstream.Handshake(r, w, []string{portforward.PortForwardProtocolV1Name}); err != nil {
				t.Error(err)
				return
			}
			conn = streamspdy.NewResponseUpgrader().UpgradeResponse(w, r, handler)
		}
		if conn != nil {
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}))
	t.Cleanup(func() {
		srv.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return srv, count
}

func TestPortForwardSession_JSONUpgradeFallback(t *testing.T) {
	for _, code := range []int{400, 404, 405} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			t.Setenv("KUBECTL_REMOTE_COMMAND_WEBSOCKETS", "true")
			backend, count := echoForwardServer(t, false)
			var gets atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					gets.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(code)
					_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: "Failure", Code: int32(code), Message: "websocket upgrade unsupported"})
					return
				}
				backend.Config.Handler.ServeHTTP(w, r)
			}))
			defer srv.Close()
			cfg, client := sessionConfig(t, srv.URL)
			h, err := StartPortForwardSession(context.Background(), cfg, client, "pod", "ns", []int{9002})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Stop()
			assertForwardEcho(t, h.Ports()[9002])
			if gets.Load() != 1 || count.Load() != 1 {
				t.Fatalf("GET=%d POST=%d, want exactly one each", gets.Load(), count.Load())
			}
			// A direct WebSocket attempt must retain the StatusError inside the
			// UpgradeFailureError, not merely turn it into a formatted string.
			target, _ := url.Parse(srv.URL)
			d := &sessionDialer{ctx: context.Background(), config: cfg, url: target}
			defer d.close()
			_, _, err = d.dialWebSocket([]string{portforward.PortForwardProtocolV1Name})
			var status *apierrors.StatusError
			if !httpstream.IsUpgradeFailure(err) || !errors.As(err, &status) || status.ErrStatus.Code != int32(code) {
				t.Fatalf("lost upgrade/status classification: %v", err)
			}
		})
	}
}

func assertForwardEcho(t *testing.T, addr string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err = c.Write([]byte("echo")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	if _, err = io.ReadFull(c, b); err != nil || string(b) != "echo" {
		t.Fatalf("echo %q: %v", b, err)
	}
}

func TestPortForwardSession_AuthenticatedTLSConnectProxy(t *testing.T) {
	for _, ws := range []bool{false, true} {
		t.Run(map[bool]string{false: "spdy", true: "websocket"}[ws], func(t *testing.T) {
			t.Setenv("KUBECTL_REMOTE_COMMAND_WEBSOCKETS", strconv.FormatBool(ws))
			backend, count := echoForwardServer(t, ws)
			pub, key, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			ca := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
			caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, pub, key)
			if err != nil {
				t.Fatal(err)
			}
			leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, DNSNames: []string{"forward.test"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
			leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, pub, key)
			if err != nil {
				t.Fatal(err)
			}
			var authenticated atomic.Int32
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer session-test-token" {
					t.Error("bearer token missing")
					http.Error(w, "unauthorized", 401)
					return
				}
				if r.TLS == nil || r.TLS.ServerName != "forward.test" {
					t.Error("custom SNI missing")
					http.Error(w, "bad SNI", 400)
					return
				}
				authenticated.Add(1)
				backend.Config.Handler.ServeHTTP(w, r)
			}))
			srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER, caDER}, PrivateKey: key}}}
			srv.StartTLS()
			defer srv.Close()
			routed := make(chan string, 1)
			proxyDone := make(chan struct{})
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodConnect || r.Host != "proxy-only.invalid:443" {
					t.Errorf("unexpected proxy request: %s %s", r.Method, r.Host)
					http.Error(w, "bad CONNECT", 400)
					return
				}
				upstream, e := net.DialTimeout("tcp", srv.Listener.Addr().String(), time.Second)
				if e != nil {
					t.Error(e)
					http.Error(w, "dial failed", 502)
					return
				}
				defer upstream.Close()
				downstream, rw, e := w.(http.Hijacker).Hijack()
				if e != nil {
					t.Error(e)
					return
				}
				defer downstream.Close()
				defer close(proxyDone)
				_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
				_ = rw.Flush()
				routed <- r.Host
				copied := make(chan struct{})
				go func() { _, _ = io.Copy(upstream, rw); _ = upstream.Close(); close(copied) }()
				_, _ = io.Copy(downstream, upstream)
				_ = downstream.Close()
				<-copied
			}))
			defer proxy.Close()
			cfg, client := sessionConfig(t, "https://proxy-only.invalid:443")
			cfg.BearerToken = "session-test-token"
			cfg.TLSClientConfig = rest.TLSClientConfig{CAData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), ServerName: "forward.test"}
			proxyURL, _ := url.Parse(proxy.URL)
			cfg.Proxy = http.ProxyURL(proxyURL)
			h, err := StartPortForwardSession(context.Background(), cfg, client, "pod", "ns", []int{10801, 10802, 9002})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Stop()
			for _, addr := range h.Ports() {
				assertForwardEcho(t, addr)
			}
			if authenticated.Load() != 1 || count.Load() != 1 {
				t.Fatalf("authenticated=%d upgrades=%d", authenticated.Load(), count.Load())
			}
			select {
			case <-routed:
			default:
				t.Fatal("CONNECT route was bypassed")
			}
			h.Stop()
			select {
			case <-proxyDone:
			case <-time.After(time.Second):
				t.Fatal("proxy socket survived Stop")
			}
		})
	}
}

func TestPortForwardSession_ThreePortsRoundTripStop(t *testing.T) {
	for _, tc := range []struct {
		name     string
		gate, ws bool
		requests int32
	}{{"spdy", false, false, 1}, {"websocket", true, true, 1}, {"upgrade-fallback", true, false, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KUBECTL_REMOTE_COMMAND_WEBSOCKETS", map[bool]string{true: "true", false: "false"}[tc.gate])
			srv, count := echoForwardServer(t, tc.ws)
			cfg, client := sessionConfig(t, srv.URL)
			var dials atomic.Int32
			cfg.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
				dials.Add(1)
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			}
			h, err := StartPortForwardSession(context.Background(), cfg, client, "pod", "ns", []int{10801, 10802, 9002})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Stop()
			endpoints := h.Ports()
			if len(endpoints) != 3 {
				t.Fatalf("ports: %v", endpoints)
			}
			var active []net.Conn
			for _, addr := range endpoints {
				host, _, _ := net.SplitHostPort(addr)
				if host != "127.0.0.1" {
					t.Fatalf("not loopback: %s", addr)
				}
				c, err := net.DialTimeout("tcp", addr, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				active = append(active, c)
				_ = c.SetDeadline(time.Now().Add(3 * time.Second))
				if _, err = c.Write([]byte("echo")); err != nil {
					t.Fatal(err)
				}
				b := make([]byte, 4)
				if _, err = io.ReadFull(c, b); err != nil || string(b) != "echo" {
					t.Fatalf("echo %q: %v", b, err)
				}
			}
			h.Stop()
			h.Stop()
			select {
			case <-h.Done():
			default:
				t.Fatal("Stop did not join")
			}
			if h.Err() != nil {
				t.Fatal(h.Err())
			}
			for _, c := range active {
				if _, err := c.Read(make([]byte, 1)); err == nil {
					t.Fatal("active connection survived Stop")
				}
			}
			for _, addr := range endpoints {
				if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
					c.Close()
					t.Fatal("listener survived Stop")
				}
			}
			if count.Load() != tc.requests {
				t.Fatalf("requests=%d, want %d", count.Load(), tc.requests)
			}
			if dials.Load() == 0 {
				t.Fatal("config.Dial bypassed")
			}
		})
	}
}

func TestPortForwardSession_UpgradeDeadlineDoesNotKillEstablished(t *testing.T) {
	for _, ws := range []bool{false, true} {
		t.Run(map[bool]string{false: "spdy", true: "websocket"}[ws], func(t *testing.T) {
			t.Setenv("KUBECTL_REMOTE_COMMAND_WEBSOCKETS", map[bool]string{false: "false", true: "true"}[ws])
			srv, _ := echoForwardServer(t, ws)
			cfg, client := sessionConfig(t, srv.URL)
			h, err := StartPortForwardSession(context.Background(), cfg, client, "pod", "ns", []int{9002})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Stop()
			timer := time.NewTimer(portForwardHandshakeTimeout + 100*time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-h.Done():
				t.Fatalf("handshake deadline killed established session: %v", h.Err())
			}
			c, err := net.DialTimeout("tcp", h.Ports()[9002], time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(time.Second))
			if _, err = c.Write([]byte("live")); err != nil {
				t.Fatal(err)
			}
			b := make([]byte, 4)
			if _, err = io.ReadFull(c, b); err != nil || string(b) != "live" {
				t.Fatalf("post-deadline echo %q: %v", b, err)
			}
		})
	}
}

func TestPortForwardSession_CancelBeforeGotConn(t *testing.T) {
	for _, stage := range []string{"tls", "connect"} {
		t.Run(stage, func(t *testing.T) {
			t.Setenv("KUBECTL_REMOTE_COMMAND_WEBSOCKETS", "false")
			accepted := make(chan struct{})
			closed := make(chan struct{})
			var cfg *rest.Config
			var client *rest.RESTClient
			if stage == "tls" {
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer ln.Close()
				go func() {
					c, e := ln.Accept()
					if e != nil {
						return
					}
					defer c.Close()
					b := make([]byte, 1)
					if _, e = c.Read(b); e != nil {
						return
					}
					close(accepted)
					_, _ = io.Copy(io.Discard, c)
					close(closed)
				}()
				cfg, client = sessionConfig(t, "https://"+ln.Addr().String())
			} else {
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodConnect {
						t.Error("expected CONNECT")
						return
					}
					c, _, e := w.(http.Hijacker).Hijack()
					if e != nil {
						t.Error(e)
						return
					}
					defer c.Close()
					close(accepted)
					_, _ = io.Copy(io.Discard, c)
					close(closed)
				}))
				defer proxy.Close()
				cfg, client = sessionConfig(t, "https://proxy-only.invalid:443")
				proxyURL, _ := url.Parse(proxy.URL)
				cfg.Proxy = http.ProxyURL(proxyURL)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				h, e := StartPortForwardSession(ctx, cfg, client, "pod", "ns", []int{9002})
				if h != nil {
					h.Stop()
				}
				result <- e
			}()
			select {
			case <-accepted:
			case <-time.After(3 * time.Second):
				t.Fatal("setup stage not reached")
			}
			cancel()
			select {
			case e := <-result:
				if !errors.Is(e, context.Canceled) {
					t.Fatalf("cancel: %v", e)
				}
			case <-time.After(time.Second):
				t.Fatal("setup ignored cancel")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("pre-GotConn socket survived cancellation")
			}
		})
	}
}

func TestPortForwardSession_CancelPendingDial(t *testing.T) {
	t.Setenv("KUBECTL_REMOTE_COMMAND_WEBSOCKETS", "false")
	cfg, client := sessionConfig(t, "https://pending.invalid")
	entered := make(chan struct{})
	dialDone := make(chan struct{})
	cfg.Dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(entered)
		<-ctx.Done()
		close(dialDone)
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		h, e := StartPortForwardSession(ctx, cfg, client, "pod", "ns", []int{9002})
		if h != nil {
			h.Stop()
		}
		result <- e
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("dial not entered")
	}
	cancel()
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("startup did not return")
	}
	select {
	case <-dialDone:
	case <-time.After(time.Second):
		t.Fatal("transport detached dial from session cancellation")
	}
}

func TestPortForwardSession_CancelStalledUpgrade(t *testing.T) {
	for _, ws := range []bool{false, true} {
		t.Run(map[bool]string{false: "spdy", true: "websocket"}[ws], func(t *testing.T) {
			t.Setenv("KUBECTL_REMOTE_COMMAND_WEBSOCKETS", map[bool]string{false: "false", true: "true"}[ws])
			accepted := make(chan struct{})
			closed := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					return
				}
				defer c.Close()
				close(accepted)
				_, _ = io.Copy(io.Discard, c)
				close(closed)
			}))
			defer srv.Close()
			cfg, client := sessionConfig(t, srv.URL)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				h, e := StartPortForwardSession(ctx, cfg, client, "pod", "ns", []int{10801, 10802, 9002})
				if h != nil {
					h.Stop()
				}
				result <- e
			}()
			select {
			case <-accepted:
			case <-time.After(3 * time.Second):
				t.Fatal("upgrade not attempted")
			}
			cancel()
			select {
			case e := <-result:
				if !errors.Is(e, context.Canceled) {
					t.Fatalf("cancel: %v", e)
				}
			case <-time.After(time.Second):
				t.Fatal("upgrade ignored cancellation")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("upgrade socket leaked")
			}
		})
	}
}

func TestPortForwardSession_PartialReadinessClosesAll(t *testing.T) {
	t.Setenv("KUBECTL_REMOTE_COMMAND_WEBSOCKETS", "false")
	srv, _ := echoForwardServer(t, false)
	cfg, client := sessionConfig(t, srv.URL)
	// The forwarder opens real listeners, but reports fewer remotes than the
	// session requires. This exercises the same cleanup as an incomplete bind.
	start := time.Now()
	h, err := startPortForwardSession(context.Background(), cfg, client, "pod", "ns", []string{"0:10801", "0:9002"}, map[int]bool{10801: true, 10802: true, 9002: true})
	if h != nil {
		h.Stop()
	}
	if err == nil {
		t.Fatal("partial readiness accepted")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("partial startup cleanup exceeded bound")
	}
}

func TestPortForwardSession_HandshakeDeadline(t *testing.T) {
	t.Setenv("KUBECTL_REMOTE_COMMAND_WEBSOCKETS", "false")
	closed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(io.Discard, c)
		close(closed)
	}))
	defer srv.Close()
	cfg, client := sessionConfig(t, srv.URL)
	start := time.Now()
	h, err := StartPortForwardSession(context.Background(), cfg, client, "pod", "ns", []int{10801, 10802, 9002})
	if h != nil {
		h.Stop()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("handshake timeout lost deadline classification: %v", err)
	}
	if time.Since(start) > portForwardHandshakeTimeout+3*time.Second {
		t.Fatal("handshake timeout exceeded")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("timed out socket survived")
	}
}

func TestPortForwardSession_StatusAndTLSNoFallback(t *testing.T) {
	for _, ws := range []bool{false, true} {
		t.Run(map[bool]string{false: "spdy", true: "websocket"}[ws], func(t *testing.T) {
			t.Setenv("KUBECTL_REMOTE_COMMAND_WEBSOCKETS", map[bool]string{false: "false", true: "true"}[ws])
			for _, code := range []int{401, 403} {
				var count atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					count.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(code)
					_ = json.NewEncoder(w).Encode(metav1.Status{Status: "Failure", Reason: map[int]metav1.StatusReason{401: metav1.StatusReasonUnauthorized, 403: metav1.StatusReasonForbidden}[code], Code: int32(code), Message: "denied"})
				}))
				cfg, client := sessionConfig(t, srv.URL)
				h, err := StartPortForwardSession(context.Background(), cfg, client, "pod", "ns", []int{9002})
				if h != nil {
					h.Stop()
				}
				srv.Close()
				if code == 403 && !apierrors.IsForbidden(err) || code == 401 && !apierrors.IsUnauthorized(err) {
					t.Fatalf("lost status %d: %v", code, err)
				}
				if count.Load() != 1 {
					t.Fatalf("auth failure retried %d times", count.Load())
				}
			}
			srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("untrusted request reached server") }))
			defer srv.Close()
			cfg, client := sessionConfig(t, srv.URL)
			h, err := StartPortForwardSession(context.Background(), cfg, client, "pod", "ns", []int{9002})
			if h != nil {
				h.Stop()
			}
			var unknown x509.UnknownAuthorityError
			if !errors.As(err, &unknown) {
				t.Fatalf("lost TLS trust error: %v", err)
			}
		})
	}
}
