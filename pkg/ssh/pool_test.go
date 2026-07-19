package ssh

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// fakeClient is an in-memory tunnelClient: it records how many times it was
// closed and returns a scripted result for each DialContext call.
type fakeClient struct {
	mu       sync.Mutex
	closed   int
	dialErr  error // if set, DialContext returns this error
	dialFunc func() (net.Conn, error)
}

func (f *fakeClient) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if f.dialFunc != nil {
		return f.dialFunc()
	}
	if f.dialErr != nil {
		return nil, f.dialErr
	}
	c, _ := net.Pipe()
	return c, nil
}

func (f *fakeClient) Close() error {
	f.mu.Lock()
	f.closed++
	f.mu.Unlock()
	return nil
}

func (f *fakeClient) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func testRemote() netip.AddrPort {
	return netip.MustParseAddrPort("10.0.0.1:6443")
}

// newTestPool builds a connPool whose client creation is fully controlled by
// create, so no real SSH server is needed.
func newTestPool(create func(ctx context.Context) (tunnelClient, error)) *connPool {
	return &connPool{
		conf: &SshConfig{},
		dialFunc: func(ctx context.Context, _ *SshConfig, _ <-chan struct{}) (tunnelClient, error) {
			return create(ctx)
		},
	}
}

// TestConnPool_KeepsClientOnContextTimeout locks the core bug fix: a per-dial
// context timeout must NOT evict/close the shared SSH client.
func TestConnPool_KeepsClientOnContextTimeout(t *testing.T) {
	fc := &fakeClient{dialErr: context.DeadlineExceeded}
	pool := newTestPool(func(context.Context) (tunnelClient, error) { return fc, nil })
	defer pool.Close()

	if _, err := pool.dial(context.Background(), testRemote()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dial err = %v, want DeadlineExceeded", err)
	}
	if fc.closeCount() != 0 {
		t.Fatalf("client closed %d times after context timeout, want 0 (client must be kept)", fc.closeCount())
	}
	// The kept client is reused on the next dial (no rebuild).
	created := 0
	pool2 := newTestPool(func(context.Context) (tunnelClient, error) { created++; return fc, nil })
	defer pool2.Close()
	_, _ = pool2.dial(context.Background(), testRemote())
	_, _ = pool2.dial(context.Background(), testRemote())
	if created != 1 {
		t.Fatalf("client created %d times across two dials, want 1", created)
	}
}

// TestConnPool_KeepsClientOnUnreachableTarget locks that an OpenChannelError
// (SSH transport alive, target unreachable) does not discard the client.
func TestConnPool_KeepsClientOnUnreachableTarget(t *testing.T) {
	fc := &fakeClient{dialErr: &gossh.OpenChannelError{Reason: gossh.ConnectionFailed, Message: "connect failed"}}
	pool := newTestPool(func(context.Context) (tunnelClient, error) { return fc, nil })
	defer pool.Close()

	if _, err := pool.dial(context.Background(), testRemote()); err == nil {
		t.Fatal("expected error for unreachable target")
	}
	if fc.closeCount() != 0 {
		t.Fatalf("client closed %d times on unreachable target, want 0", fc.closeCount())
	}
}

// TestConnPool_EvictsDeadClient locks that a transport-level failure (EOF) evicts
// and closes the client so the next dial rebuilds a fresh one.
func TestConnPool_EvictsDeadClient(t *testing.T) {
	dead := &fakeClient{dialErr: io.EOF}
	healthy := &fakeClient{}
	var created int32
	pool := newTestPool(func(context.Context) (tunnelClient, error) {
		if atomic.AddInt32(&created, 1) == 1 {
			return dead, nil
		}
		return healthy, nil
	})
	defer pool.Close()

	if _, err := pool.dial(context.Background(), testRemote()); !errors.Is(err, io.EOF) {
		t.Fatalf("first dial err = %v, want EOF", err)
	}
	if dead.closeCount() != 1 {
		t.Fatalf("dead client closed %d times, want 1", dead.closeCount())
	}
	// Second dial must rebuild and succeed against the healthy client.
	conn, err := pool.dial(context.Background(), testRemote())
	if err != nil {
		t.Fatalf("second dial err = %v, want nil", err)
	}
	_ = conn.Close()
	if atomic.LoadInt32(&created) != 2 {
		t.Fatalf("client created %d times, want 2 (rebuild after eviction)", created)
	}
}

// TestConnPool_SingleFlightCreation locks that a burst of concurrent dials builds
// the SSH client exactly once instead of racing to open one each.
func TestConnPool_SingleFlightCreation(t *testing.T) {
	var created int32
	pool := newTestPool(func(context.Context) (tunnelClient, error) {
		atomic.AddInt32(&created, 1)
		time.Sleep(20 * time.Millisecond) // widen the race window
		return &fakeClient{}, nil
	})
	defer pool.Close()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := pool.dial(context.Background(), testRemote())
			if err == nil {
				_ = conn.Close()
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&created); got != 1 {
		t.Fatalf("client created %d times under concurrent dials, want 1 (single-flight)", got)
	}
}

// TestConnPool_CloseTearsDownClient locks that Close discards the shared client.
func TestConnPool_CloseTearsDownClient(t *testing.T) {
	fc := &fakeClient{}
	pool := newTestPool(func(context.Context) (tunnelClient, error) { return fc, nil })
	conn, err := pool.dial(context.Background(), testRemote())
	if err != nil {
		t.Fatalf("dial err = %v", err)
	}
	_ = conn.Close()
	pool.Close()
	if fc.closeCount() != 1 {
		t.Fatalf("client closed %d times after pool.Close, want 1", fc.closeCount())
	}
	// Double Close is safe.
	pool.Close()
	if fc.closeCount() != 1 {
		t.Fatalf("client closed %d times after second pool.Close, want 1", fc.closeCount())
	}
}
