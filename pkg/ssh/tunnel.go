package ssh

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"

	gossh "golang.org/x/crypto/ssh"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// PortMap owns a listener, its SSH pool, and all accepted forwarding workers.
// Err is available after Done closes. Stop is idempotent and joins the workers.
type PortMap struct {
	listener net.Listener
	cancel   context.CancelFunc
	done     chan struct{}
	err      error
}

func (p *PortMap) Addr() net.Addr        { return p.listener.Addr() }
func (p *PortMap) Done() <-chan struct{} { return p.done }
func (p *PortMap) Err() error            { return p.err }
func (p *PortMap) Stop()                 { p.cancel(); <-p.done }

// StartPortMap starts a lazy SSH forward. A zero local port is allocated by the OS.
func StartPortMap(ctx context.Context, conf *SshConfig, remote, local netip.AddrPort) (*PortMap, error) {
	ctx, cancel := context.WithCancel(ctx)
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", local.String())
	if err != nil {
		cancel()
		return nil, err
	}
	p := &PortMap{listener: ln, cancel: cancel, done: make(chan struct{})}
	pool := newConnPool(conf)
	go func() {
		defer close(p.done)
		var workers sync.WaitGroup
		var mu sync.Mutex
		active := map[net.Conn]struct{}{}
		shutdown := make(chan struct{})
		go func() {
			defer close(shutdown)
			<-ctx.Done()
			_ = ln.Close()
			mu.Lock()
			for c := range active {
				_ = c.Close()
			}
			mu.Unlock()
			pool.Close()
		}()
		var failOnce sync.Once
		fail := func(err error) { failOnce.Do(func() { p.err = err; cancel() }) }
		for {
			c, err := ln.Accept()
			if err != nil {
				if ctx.Err() == nil {
					fail(err)
				}
				break
			}
			mu.Lock()
			if ctx.Err() != nil {
				mu.Unlock()
				_ = c.Close()
				break
			}
			active[c] = struct{}{}
			mu.Unlock()
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer func() { _ = c.Close(); mu.Lock(); delete(active, c); mu.Unlock() }()
				r, err := pool.dial(ctx, remote)
				if err != nil {
					var oe *gossh.OpenChannelError
					if (errors.As(err, &oe) && oe.Reason == gossh.Prohibited) || errors.Is(err, config.ErrSSHAuth) || errors.Is(err, config.ErrGSSAPI) {
						fail(err)
					} else {
						plog.G(ctx).Debugf("failed to dial SSH target %s: %v", remote, err)
					}
					return
				}
				// copyStream closes and joins both directions even if cancellation raced dial.
				copyStream(ctx, c, r)
			}()
		}
		cancel()
		<-shutdown
		workers.Wait()
	}()
	return p, nil
}

// PortMapUntil preserves the asynchronous, context-owned API used by existing callers.
func PortMapUntil(ctx context.Context, conf *SshConfig, remote, local netip.AddrPort) error {
	_, err := StartPortMap(ctx, conf, remote, local)
	return err
}

func copyStream(ctx context.Context, local net.Conn, remote net.Conn) {
	done := make(chan struct{}, 2)
	copy := func(dst, src net.Conn, direction string) {
		defer func() { done <- struct{}{} }()
		buf := config.LPool.Get().([]byte)[:]
		defer config.LPool.Put(buf[:])
		_, err := io.CopyBuffer(dst, src, buf)
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
			plog.G(ctx).Debugf("copy %s error: %v", direction, err)
		}
	}
	go copy(local, remote, "remote->local")
	go copy(remote, local, "local->remote")
	remaining := 2
	select {
	case <-done:
		remaining--
	case <-ctx.Done():
	}
	_ = local.Close()
	_ = remote.Close()
	for ; remaining > 0; remaining-- {
		<-done
	}
}
