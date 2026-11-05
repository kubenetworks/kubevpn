package endpoint_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/endpoint"
	framing "github.com/wencaiwulue/kubevpn/v2/pkg/internal/wire"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tunnel"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

// setTestTLSEnv provisions the self-signed TLS material the gtcp listener and
// transporter read from the environment: the data plane never falls back to
// raw TCP, so tests exercising real listeners/transporters must provide it.
func setTestTLSEnv(t *testing.T) {
	t.Helper()
	crt, key, host, err := netutil.GenTLSCert(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.TLSCertKey, string(crt))
	t.Setenv(config.TLSPrivateKeyKey, string(key))
	t.Setenv(config.TLSServerName, string(host))
}

// A packet-device surrogate with an existing loopback address exercises real
// device discovery and tunnel.TunHandler/serve without changing any network settings.
type serverShutdownTUN struct {
	net.Conn
	read chan struct{}
	fail chan struct{}
	once sync.Once
}

func (c *serverShutdownTUN) LocalAddr() net.Addr { return &net.IPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (c *serverShutdownTUN) Read([]byte) (int, error) {
	c.once.Do(func() { close(c.read) })
	<-c.fail
	return 0, io.ErrUnexpectedEOF
}

type serverShutdownListener struct {
	net.Listener
	conn net.Conn
	once sync.Once
}

func (l *serverShutdownListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.conn })
	if c != nil {
		return c, nil
	}
	return l.Listener.Accept()
}

// TestServe_ObfuscatedOverTLS exercises the full production client/server
// layering — TCP, TLS, obfuscating stream, datagram framing — over a real
// loopback connection. The obfuscating wrapper must sit transparently between
// the TLS record layer and the framing layer in both directions.
func TestServe_ObfuscatedOverTLS(t *testing.T) {
	setTestTLSEnv(t)
	t.Setenv(config.ObfuscationKey, "1")
	t.Cleanup(func() { framing.SetObfuscation(false) })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	serverTLS, err := netutil.GetTlsServerConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	tlsLn := tls.NewListener(ln, serverTLS)

	serverDone := make(chan error, 1)
	go func() {
		conn, err := tlsLn.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		// Production server stack: obfuscation below framing.
		framed, err := framing.NewConn(context.Background(), framing.ObfuscateAccept(conn))
		if err != nil {
			_ = conn.Close()
			serverDone <- err
			return
		}
		buf := make([]byte, 128*1024)
		for i := 0; i < 3; i++ {
			n, err := framed.Read(buf)
			if err != nil {
				serverDone <- err
				return
			}
			if _, err := framed.Write(buf[:n]); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- framed.Close()
	}()

	// Production client stack: transporter (sets the flag from env) +
	// connector (wraps below framing).
	tr := endpoint.TCPTransporter(nil)
	if !framing.ObfuscationEnabled() {
		t.Fatal("TCPTransporter did not enable obfuscation from env")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := tr.Dial(ctx, tlsLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	framed, err := endpoint.NewUDPOverTCPConnector().ConnectContext(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}

	msgs := [][]byte{
		bytes.Repeat([]byte{0x11}, 32),
		bytes.Repeat([]byte{0x22}, 2048),
		bytes.Repeat([]byte{0x33}, 65535),
	}
	echo := make([]byte, 128*1024)
	for _, m := range msgs {
		if _, err := framed.Write(m); err != nil {
			t.Fatalf("client Write: %v", err)
		}
		n, err := framed.Read(echo)
		if err != nil {
			t.Fatalf("client Read: %v", err)
		}
		if !bytes.Equal(echo[:n], m) {
			t.Fatalf("echo mismatch at len=%d (got %d)", len(m), n)
		}
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server goroutine: %v", err)
	}
}

func TestStage3_RunServerTUNFatal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	raw, peer := net.Pipe()
	defer raw.Close()
	defer peer.Close()
	tun := &serverShutdownTUN{Conn: raw, read: make(chan struct{}), fail: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- endpoint.Serve(ctx, []endpoint.Server{{Listener: &serverShutdownListener{Listener: ln, conn: tun}, Handler: tunnel.TunHandler(nil, endpoint.NewRouteHub(), nil)}})
	}()
	select {
	case <-tun.read:
	case <-time.After(3 * time.Second):
		close(tun.fail)
		cancel()
		<-done
		t.Fatal("real tunnel.TunHandler did not start device reader")
	}
	close(tun.fail)
	select {
	case err := <-done:
		if err == nil || errors.Is(err, context.Canceled) {
			t.Fatalf("fatal return missing: %v", err)
		}
		if ctx.Err() != nil {
			t.Fatal("test parent was canceled")
		}
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("fatal TUN exit did not stop Run")
	}
}

// lifecycleHandler wraps the real tunnel handler to observe service sequencing.
type lifecycleHandler struct {
	endpoint.Handler
	service interface {
		Start(context.Context)
		Close() error
	}
	started chan struct{}
	closed  chan struct{}
}

func (h *lifecycleHandler) Start(ctx context.Context) { h.service.Start(ctx); close(h.started) }
func (h *lifecycleHandler) Close() error              { err := h.service.Close(); close(h.closed); return err }
func (h *lifecycleHandler) Handle(ctx context.Context, c net.Conn) {
	select {
	case <-h.started:
	default:
		panic("accepted before service start")
	}
	h.Handler.Handle(ctx, c)
}

func TestServe_StartsAndJoinsRealTunnelService(t *testing.T) {
	setTestTLSEnv(t)
	servers, err := endpoint.GenerateServers([]string{"gtcp://127.0.0.1:0"}, endpoint.NewRouteHub())
	if err != nil {
		t.Fatal(err)
	}
	real := servers[0].Handler
	service, ok := real.(interface {
		Start(context.Context)
		Close() error
	})
	if !ok {
		t.Fatal("factory handler lacks service lifecycle")
	}
	observed := &lifecycleHandler{Handler: real, service: service, started: make(chan struct{}), closed: make(chan struct{})}
	servers[0].Handler = observed
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- endpoint.Serve(ctx, servers) }()
	select {
	case <-observed.started:
	case <-time.After(3 * time.Second):
		t.Fatal("service not started")
	}
	conn, err := net.Dial("tcp", servers[0].Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("service not joined")
	}
	select {
	case <-observed.closed:
	default:
		t.Fatal("Serve returned before service Close")
	}
}

type serverShutdownEOFHandler struct{ done chan struct{} }

func (h serverShutdownEOFHandler) Handle(_ context.Context, c net.Conn) {
	io.Copy(io.Discard, c)
	h.done <- struct{}{}
}
func TestStage3_RunOrdinaryEOFIsNotFatal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handled := make(chan struct{}, 2)
	done := make(chan error, 1)
	go func() {
		done <- endpoint.Serve(ctx, []endpoint.Server{{Listener: ln, Handler: serverShutdownEOFHandler{done: handled}}})
	}()
	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
		select {
		case <-handled:
		case <-time.After(time.Second):
			t.Fatal("ordinary handler did not finish")
		}
	}
	select {
	case err := <-done:
		t.Fatalf("ordinary EOF terminated Run: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not join")
	}
}
