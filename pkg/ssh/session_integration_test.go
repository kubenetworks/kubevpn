package ssh

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	gossh "golang.org/x/crypto/ssh"
)

// loopbackSSH serves real SSH direct-tcpip channels; no cluster or user keys.
func loopbackSSH(t *testing.T, reject gossh.RejectionReason, denyAuth bool) *SshConfig {
	t.Helper()
	return loopbackSSHWithChannel(t, reject, denyAuth, func(stream gossh.Channel) { _, _ = io.Copy(stream, stream) })
}

func loopbackSSHWithChannel(t *testing.T, reject gossh.RejectionReason, denyAuth bool, handle func(gossh.Channel)) *SshConfig {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &gossh.ServerConfig{PasswordCallback: func(gossh.ConnMetadata, []byte) (*gossh.Permissions, error) {
		if denyAuth {
			return nil, errors.New("denied")
		}
		return nil, nil
	}}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	conns := map[net.Conn]bool{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			mu.Lock()
			conns[c] = true
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
						_ = ch.Reject(reject, "test rejection")
						continue
					}
					stream, requests, e := ch.Accept()
					if e != nil {
						return
					}
					wg.Add(1)
					go func() {
						defer wg.Done()
						defer stream.Close()
						go gossh.DiscardRequests(requests)
						handle(stream)
					}()
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return &SshConfig{Addr: ln.Addr().String(), User: "test", Password: "test"}
}

func awaitSSH(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("SSH worker did not join")
	}
}

func TestPortMapIntegration_RoundTripStop(t *testing.T) {
	conf := loopbackSSH(t, 0, false)
	h, err := StartPortMap(context.Background(), conf, netip.MustParseAddrPort("127.0.0.1:9000"), netip.MustParseAddrPort("127.0.0.1:0"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop()
	c, err := net.DialTimeout("tcp", h.Addr().String(), time.Second)
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
	h.Stop()
	awaitSSH(t, h.Done())
	h.Stop()
	if h.Err() != nil {
		t.Fatal(h.Err())
	}
	if c2, e := net.DialTimeout("tcp", h.Addr().String(), time.Second); e == nil {
		c2.Close()
		t.Fatal("listener survives Stop")
	}
	if _, e := c.Read(b); e == nil {
		t.Fatal("active connection survives Stop")
	}
}

func TestPortMapIntegration_TerminalFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason gossh.RejectionReason
		auth   bool
	}{{"prohibited", gossh.Prohibited, false}, {"auth", 0, true}} {
		t.Run(tc.name, func(t *testing.T) {
			conf := loopbackSSH(t, tc.reason, tc.auth)
			h, err := StartPortMap(context.Background(), conf, testRemote(), netip.MustParseAddrPort("127.0.0.1:0"))
			if err != nil {
				t.Fatal(err)
			}
			defer h.Stop()
			c, err := net.Dial("tcp", h.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			awaitSSH(t, h.Done())
			if tc.auth {
				if !errors.Is(h.Err(), config.ErrSSHAuth) {
					t.Fatalf("classification: %v", h.Err())
				}
			} else {
				var oe *gossh.OpenChannelError
				if !errors.As(h.Err(), &oe) || oe.Reason != gossh.Prohibited {
					t.Fatalf("classification: %v", h.Err())
				}
			}
			if c2, e := net.DialTimeout("tcp", h.Addr().String(), time.Second); e == nil {
				c2.Close()
				t.Fatal("terminal failure left listener live")
			}
		})
	}
}

func TestDialIntegration_CancelHangingHandshake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, e := ln.Accept()
		if e == nil {
			accepted <- c
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		c, e := (&SshConfig{Addr: ln.Addr().String(), User: "test", Password: "test"}).Dial(ctx, nil)
		if c != nil {
			c.Close()
		}
		result <- e
	}()
	c := <-accepted
	defer c.Close()
	cancel()
	select {
	case e := <-result:
		if !errors.Is(e, config.ErrSSHConnect) {
			t.Fatalf("classification: %v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not interrupt SSH handshake")
	}
}

func TestConnPool_CloseIsTerminal(t *testing.T) {
	conf := loopbackSSH(t, 0, false)
	p := newConnPool(conf)
	c, err := p.dial(context.Background(), testRemote())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() { defer wg.Done(); p.Close() }()
	}
	wg.Wait()
	c, err = p.dial(context.Background(), testRemote())
	if c != nil {
		c.Close()
	}
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed pool recreated client: %v", err)
	}
}

func TestJumpToIntegration_CancelHandshake(t *testing.T) {
	testJumpToStalledHandshake(t, true)
}

func TestJumpToIntegration_HandshakeDeadline(t *testing.T) {
	testJumpToStalledHandshake(t, false)
}

func testJumpToStalledHandshake(t *testing.T, cancelHandshake bool) {
	t.Helper()
	accepted := make(chan struct{})
	peerClosed := make(chan struct{})
	conf := loopbackSSHWithChannel(t, 0, false, func(stream gossh.Channel) {
		defer close(peerClosed)
		reader := bufio.NewReader(stream)
		banner, err := reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(banner, "SSH-2.0-") {
			t.Errorf("jump handshake banner %q: %v", banner, err)
			return
		}
		close(accepted)
		// Deliberately withhold the server banner, leaving NewClientConn blocked.
		_, _ = io.Copy(io.Discard, reader)
	})
	base, err := conf.Dial(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	result := make(chan error, 1)
	go func() {
		c, e := JumpTo(ctx, base, *conf, nil)
		if c != nil {
			c.Close()
		}
		result <- e
	}()
	awaitSSH(t, accepted)
	wait := sshDialTimeout + time.Second
	if cancelHandshake {
		cancel()
		wait = time.Second
	}
	select {
	case e := <-result:
		if !errors.Is(e, config.ErrSSHConnect) {
			t.Fatalf("jump classification: %v", e)
		}
	case <-time.After(wait):
		t.Fatal("accepted jump handshake ignored cancellation/deadline")
	}
	if !cancelHandshake && time.Since(start) < sshDialTimeout {
		t.Fatal("handshake failed before its configured deadline")
	}
	awaitSSH(t, peerClosed)
	// Failure releases both the accepted handshake channel and its bastion.
	ended := make(chan struct{})
	go func() { _ = base.Wait(); close(ended) }()
	awaitSSH(t, ended)
}

func TestConnPool_CloseDuringHandshake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, e := ln.Accept()
		if e == nil {
			accepted <- c
		}
	}()
	p := newConnPool(&SshConfig{Addr: ln.Addr().String(), User: "test", Password: "test"})
	result := make(chan error, 1)
	go func() {
		c, e := p.dial(context.Background(), testRemote())
		if c != nil {
			c.Close()
		}
		result <- e
	}()
	c := <-accepted
	defer c.Close()
	joined := make(chan struct{})
	go func() { p.Close(); close(joined) }()
	awaitSSH(t, joined)
	if e := <-result; !errors.Is(e, net.ErrClosed) {
		t.Fatalf("close during creation: %v", e)
	}
}

func TestPortMapIntegration_UnreachableIsPerConnection(t *testing.T) {
	conf := loopbackSSH(t, gossh.ConnectionFailed, false)
	h, err := StartPortMap(context.Background(), conf, testRemote(), netip.MustParseAddrPort("127.0.0.1:0"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop()
	for range 2 {
		c, e := net.DialTimeout("tcp", h.Addr().String(), time.Second)
		if e != nil {
			t.Fatal(e)
		}
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		_, e = c.Read(make([]byte, 1))
		c.Close()
		if e == nil {
			t.Fatal("unreachable channel remained open")
		}
		select {
		case <-h.Done():
			t.Fatalf("target rejection killed session: %v", h.Err())
		default:
		}
	}
}

// Closing an SSH channel can block writing to its stalled parent transport.
type parentBlockedCloseConn struct {
	net.Conn
	parentClosed <-chan struct{}
}

func (c *parentBlockedCloseConn) Close() error { <-c.parentClosed; return nil }
func TestOwnedSSHConn_ClosesParentBeforeChild(t *testing.T) {
	for _, cancelFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "close", true: "cancel"}[cancelFirst], func(t *testing.T) {
			parentClosed := make(chan struct{})
			var once sync.Once
			closeParent := func() error { once.Do(func() { close(parentClosed) }); return nil }
			defer closeParent()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := watchSSHConn(ctx, nil, &parentBlockedCloseConn{parentClosed: parentClosed}, closeParent)
			done := make(chan struct{})
			if cancelFirst {
				cancel()
				go func() { <-c.(*ownedSSHConn).joined; close(done) }()
			} else {
				go func() { _ = c.Close(); close(done) }()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("child Close blocked before parent was closed")
			}
		})
	}
}

// Hold both reads after Close to prove cancellation joins both copy workers.
type heldReadConn struct {
	net.Conn
	entered chan struct{}
	release <-chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (c *heldReadConn) Read([]byte) (int, error) {
	close(c.entered)
	<-c.closed
	<-c.release
	return 0, net.ErrClosed
}
func (c *heldReadConn) Close() error { c.once.Do(func() { close(c.closed) }); return nil }
func TestCopyStream_CancelJoinsBothWorkers(t *testing.T) {
	release1, release2 := make(chan struct{}), make(chan struct{})
	a := &heldReadConn{entered: make(chan struct{}), closed: make(chan struct{}), release: release1}
	b := &heldReadConn{entered: make(chan struct{}), closed: make(chan struct{}), release: release2}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { copyStream(ctx, a, b); close(done) }()
	<-a.entered
	<-b.entered
	cancel()
	<-a.closed
	<-b.closed
	close(release1)
	select {
	case <-done:
		close(release2)
		t.Fatal("copyStream returned while second worker was blocked")
	case <-time.After(30 * time.Millisecond):
	}
	close(release2)
	awaitSSH(t, done)
}
