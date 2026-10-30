package handler

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
)

// A packet-device surrogate with an existing loopback address exercises real
// device discovery and TunHandler/serve without changing any network settings.
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
		done <- Run(ctx, []core.Server{{Listener: &serverShutdownListener{Listener: ln, conn: tun}, Handler: core.TunHandler(nil, core.NewRouteHub(), nil)}})
	}()
	select {
	case <-tun.read:
	case <-time.After(3 * time.Second):
		close(tun.fail)
		cancel()
		<-done
		t.Fatal("real TunHandler did not start device reader")
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
		done <- Run(ctx, []core.Server{{Listener: ln, Handler: serverShutdownEOFHandler{done: handled}}})
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
