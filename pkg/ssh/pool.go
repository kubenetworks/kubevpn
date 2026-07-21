package ssh

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"

	gossh "golang.org/x/crypto/ssh"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// tunnelClient is the minimal surface of an SSH client the pool depends on:
// dialing a target through the tunnel and closing the connection. *gossh.Client
// satisfies it; tests inject fakes.
type tunnelClient interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
	Close() error
}

func newSshClientWrap(client tunnelClient, cancel context.CancelFunc) *sshClientWrap {
	return &sshClientWrap{tunnelClient: client, cancel: cancel}
}

// sshClientWrap couples an SSH client with the cancel func of the context that
// scopes its lifetime, so closing it also tears down that context.
type sshClientWrap struct {
	cancel context.CancelFunc
	tunnelClient
}

func (c *sshClientWrap) Close() error {
	c.cancel()
	return c.tunnelClient.Close()
}

// connPool manages a single, lazily-established SSH client used to dial targets
// through an SSH tunnel. Compared with the previous per-connection sync.Map it
// guarantees two properties:
//
//   - single-flight creation: the client is built at most once even under a burst
//     of concurrent dials, instead of every cold-start connection racing to open
//     its own SSH client.
//   - health-aware eviction: the client is discarded ONLY when the underlying SSH
//     transport is genuinely dead. A per-dial context cancellation/timeout, or a
//     target that is merely unreachable (the SSH server answers with an
//     OpenChannelError), leaves the client intact for reuse — avoiding the
//     reconnect storm that previously turned a slow target into a cascade of
//     bastion re-handshakes.
type connPool struct {
	conf     *SshConfig
	dialFunc func(context.Context, *SshConfig, <-chan struct{}) (tunnelClient, error)

	mu     sync.Mutex
	client *sshClientWrap
}

func newConnPool(conf *SshConfig) *connPool {
	return &connPool{
		conf: conf,
		dialFunc: func(ctx context.Context, c *SshConfig, stop <-chan struct{}) (tunnelClient, error) {
			return DialSshRemote(ctx, c, stop)
		},
	}
}

// dial returns a TCP connection to remote, tunneled through the pool's SSH client,
// creating that client on first use. The lifetime context is ctx: the client stays
// alive until ctx is cancelled (or until it is evicted as dead).
func (p *connPool) dial(ctx context.Context, remote netip.AddrPort) (net.Conn, error) {
	client, err := p.getClient(ctx)
	if err != nil {
		return nil, err
	}

	ctx1, cancel := context.WithTimeout(ctx, sshTunnelDialTimeout)
	defer cancel()
	conn, err := client.DialContext(ctx1, "tcp", remote.String())
	if err == nil {
		return conn, nil
	}

	// Classify the failure to decide whether the SSH client is still usable.
	var openErr *gossh.OpenChannelError
	switch {
	case isContextError(err):
		// This dial was cancelled or timed out; the SSH client itself may be
		// perfectly healthy, so keep it for reuse.
	case errors.As(err, &openErr):
		// The SSH transport answered (it returned a channel-open error), so the
		// client is alive; the target is unreachable or forbidden. Keep the client
		// — rebuilding it would not help and only adds a bastion re-handshake.
	default:
		// A transport-level failure (EOF, use of closed connection): the client is
		// dead. Discard it so the next dial rebuilds a fresh one.
		plog.G(ctx).Debugf("discarding dead SSH client after dial to %s failed: %v", remote.String(), err)
		p.evict(client)
	}
	return nil, err
}

// getClient returns the shared SSH client, building it on first use. Creation is
// serialized by p.mu so concurrent callers reuse a single client (single-flight).
func (p *connPool) getClient(ctx context.Context) (*sshClientWrap, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return p.client, nil
	}

	// Scope the client to its own cancelable context so it can be torn down
	// independently on eviction, or together with the tunnel when ctx is done.
	clientCtx, cancel := context.WithCancel(ctx)
	client, err := p.dialFunc(clientCtx, p.conf, clientCtx.Done())
	if err != nil {
		cancel()
		plog.G(ctx).Debugf("failed to dial remote SSH server: %v", err)
		return nil, err
	}
	wrap := newSshClientWrap(client, cancel)
	p.client = wrap
	plog.G(ctx).Debug("connected to remote SSH server")

	go func() {
		<-ctx.Done()
		p.evict(wrap)
	}()
	return wrap, nil
}

// evict removes w if it is still the current client and closes it. It is safe to
// call multiple times for the same client — only the first call closes it.
func (p *connPool) evict(w *sshClientWrap) {
	p.mu.Lock()
	removed := p.client == w
	if removed {
		p.client = nil
	}
	p.mu.Unlock()
	if removed {
		_ = w.Close()
	}
}

// Close tears down the pool's SSH client, if any.
func (p *connPool) Close() {
	p.mu.Lock()
	w := p.client
	p.client = nil
	p.mu.Unlock()
	if w != nil {
		_ = w.Close()
	}
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
