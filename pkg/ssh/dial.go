package ssh

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/spf13/pflag"
	gossh "golang.org/x/crypto/ssh"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

const (
	// sshDialTimeout bounds the TCP dial and SSH handshake to a bastion/jump host.
	sshDialTimeout = 10 * time.Second
	// sshTunnelDialTimeout bounds a single dial to a target *through* an established
	// SSH tunnel. It is deliberately shorter than the kube client's TLS-handshake
	// timeout (10s, k8s.io/client-go/transport) so that when a bastion cannot reach
	// the target the failure surfaces as a clear tunnel error instead of an opaque
	// "net/http: TLS handshake timeout".
	sshTunnelDialTimeout = 8 * time.Second
)

// DialSshRemote establishes an SSH client connection, resolving aliases and jump hosts recursively.
func DialSshRemote(ctx context.Context, conf *SshConfig, stopChan <-chan struct{}) (client *gossh.Client, err error) {
	defer func() {
		if err != nil && client != nil {
			client.Close()
		}
	}()

	if conf.ConfigAlias != "" {
		return conf.AliasRecursion(ctx, stopChan)
	}
	if conf.Jump != "" {
		return conf.JumpRecursion(ctx, stopChan)
	}
	return conf.Dial(ctx, stopChan)
}

// JumpTo dials through an existing SSH client (bastion) to establish a new SSH connection to the target host.
func JumpTo(ctx context.Context, bClient *gossh.Client, to SshConfig, stopChan <-chan struct{}) (client *gossh.Client, err error) {
	defer func() {
		if err != nil {
			_ = bClient.Close()
		}
	}()
	if _, _, err = net.SplitHostPort(to.Addr); err != nil {
		// use default ssh port 22
		to.Addr = net.JoinHostPort(to.Addr, "22")
		err = nil
	}

	var authMethod []gossh.AuthMethod
	authMethod, err = to.GetAuth()
	if err != nil {
		return nil, err
	}
	// Dial a connection to the service host, from the bastion
	var conn net.Conn
	dialCtx, dialCancel := context.WithTimeout(ctx, sshDialTimeout)
	conn, err = bClient.DialContext(dialCtx, "tcp", to.Addr)
	dialCancel()
	if err != nil {
		err = fmt.Errorf("dial %s via bastion: %w: %w", to.Addr, err, config.ErrSSHConnect)
		return
	}
	conn = watchSSHConn(ctx, stopChan, conn, bClient.Close)
	finishHandshake := boundSSHHandshake(conn)
	defer finishHandshake()
	defer func() {
		if err != nil {
			if client != nil {
				client.Close()
			}
			if conn != nil {
				conn.Close()
			}
		}
	}()
	var ncc gossh.Conn
	var chans <-chan gossh.NewChannel
	var reqs <-chan *gossh.Request
	ncc, chans, reqs, err = gossh.NewClientConn(conn, to.Addr, &gossh.ClientConfig{
		User:            to.User,
		Auth:            authMethod,
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		//BannerCallback:  ssh.BannerDisplayStderr(),
		Timeout: sshDialTimeout,
	})
	if err != nil {
		err = wrapDialError(err)
		return
	}

	client = gossh.NewClient(ncc, chans, reqs)
	return
}

func (conf SshConfig) AliasRecursion(ctx context.Context, stopChan <-chan struct{}) (client *gossh.Client, err error) {
	list := getDefaultSSHConfigList()
	bastionList, err := resolveProxyJumpChain(conf.ConfigAlias, conf, list)
	if err != nil {
		return nil, err
	}
	for i := len(bastionList) - 1; i >= 0; i-- {
		if client == nil {
			client, err = bastionList[i].Dial(ctx, stopChan)
			if err != nil {
				err = fmt.Errorf("failed to connect to %v: %w", bastionList[i], err)
				return
			}
		} else {
			client, err = JumpTo(ctx, client, bastionList[i], stopChan)
			if err != nil {
				err = fmt.Errorf("failed to jump to %v: %w", bastionList[i], err)
				return
			}
		}
	}
	return
}

func (conf SshConfig) JumpRecursion(ctx context.Context, stopChan <-chan struct{}) (client *gossh.Client, err error) {
	flags := pflag.NewFlagSet("", pflag.ContinueOnError)
	sshConf := &SshConfig{}
	AddSshFlags(flags, sshConf)
	err = flags.Parse(strings.Split(conf.Jump, " "))
	if err != nil {
		return nil, fmt.Errorf("parse SSH jump %q: %w: %w", conf.Jump, err, config.ErrSSHConfig)
	}
	var baseClient *gossh.Client
	baseClient, err = DialSshRemote(ctx, sshConf, stopChan)
	if err != nil {
		return nil, err
	}
	// Close baseClient only on error; on success it is the transport for the
	// jumped client (or the returned client itself when there is no further hop).
	defer func() {
		if err != nil && baseClient != nil {
			_ = baseClient.Close()
		}
	}()

	var bastionList []SshConfig
	if conf.ConfigAlias != "" {
		list := getDefaultSSHConfigList()
		bastionList, err = resolveProxyJumpChain(conf.ConfigAlias, conf, list)
		if err != nil {
			return nil, err
		}
	}
	if conf.Addr != "" {
		bastionList = append(bastionList, conf)
	}

	// No further hop configured: the jump host itself is the target.
	if len(bastionList) == 0 {
		return baseClient, nil
	}

	for _, sshConfig := range bastionList {
		client, err = JumpTo(ctx, baseClient, sshConfig, stopChan)
		if err != nil {
			err = fmt.Errorf("failed to jump to %s: %w", sshConfig, err)
			return
		}
	}
	return
}

func (conf SshConfig) Dial(ctx context.Context, stopChan <-chan struct{}) (client *gossh.Client, err error) {
	if _, _, err = net.SplitHostPort(conf.Addr); err != nil {
		// use default ssh port 22
		conf.Addr = net.JoinHostPort(conf.Addr, "22")
		err = nil
	}
	// connect to the bastion host
	authMethod, err := conf.GetAuth()
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: sshDialTimeout, KeepAlive: config.KeepAliveTime}
	conn, err := d.DialContext(ctx, "tcp", conf.Addr)
	if err != nil {
		return nil, fmt.Errorf("dial SSH %s: %w: %w", conf.Addr, err, config.ErrSSHConnect)
	}
	conn = watchSSHConn(ctx, stopChan, conn, nil)
	finishHandshake := boundSSHHandshake(conn)
	defer finishHandshake()
	defer func() {
		if err != nil {
			if conn != nil {
				conn.Close()
			}
			if client != nil {
				client.Close()
			}
		}
	}()
	c, chans, reqs, err := gossh.NewClientConn(conn, conf.Addr, &gossh.ClientConfig{
		User:            conf.User,
		Auth:            authMethod,
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		//BannerCallback:  ssh.BannerDisplayStderr(),
		Timeout: sshDialTimeout,
	})
	if err != nil {
		return nil, wrapDialError(err)
	}
	return gossh.NewClient(c, chans, reqs), nil
}

// ownedSSHConn releases its cancellation watcher when the client is closed.
type ownedSSHConn struct {
	net.Conn
	once        sync.Once
	done        chan struct{}
	joined      chan struct{}
	closeParent func() error
}

func watchSSHConn(ctx context.Context, stop <-chan struct{}, conn net.Conn, closeParent func() error) net.Conn {
	c := &ownedSSHConn{Conn: conn, done: make(chan struct{}), joined: make(chan struct{}), closeParent: closeParent}
	go func() {
		defer close(c.joined)
		select {
		case <-ctx.Done():
		case <-stop:
		case <-c.done:
			return
		}
		_ = c.closeResources()
	}()
	return c
}

func (c *ownedSSHConn) Close() error {
	err := c.closeResources()
	<-c.joined
	return err
}

func (c *ownedSSHConn) closeResources() error {
	var err error
	c.once.Do(func() {
		close(c.done)
		if c.closeParent != nil {
			_ = c.closeParent()
		}
		err = c.Conn.Close()
	})
	return err
}

func (c *ownedSSHConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if err != nil {
		_ = c.Close()
	}
	return n, err
}

// ClientConfig.Timeout does not bound NewClientConn. Set a real socket deadline
// and also close on timeout for SSH channel conns, which lack deadline support.
func boundSSHHandshake(conn net.Conn) func() {
	_ = conn.SetDeadline(time.Now().Add(sshDialTimeout))
	joined := make(chan struct{})
	timer := time.AfterFunc(sshDialTimeout, func() { defer close(joined); _ = conn.Close() })
	return func() {
		if !timer.Stop() {
			<-joined
		}
		_ = conn.SetDeadline(time.Time{})
	}
}

// wrapDialError classifies an SSH dial/handshake error for exit-code purposes.
// x/crypto/ssh exposes no typed authentication error, so an auth failure is detected
// by its message text; GSSAPI/Kerberos token failures surface during the handshake too.
func wrapDialError(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "kerberos") || strings.Contains(msg, "gssapi"):
		return fmt.Errorf("%w: %w", err, config.ErrGSSAPI)
	case strings.Contains(msg, "unable to authenticate"):
		return fmt.Errorf("%w: %w", err, config.ErrSSHAuth)
	default:
		return fmt.Errorf("%w: %w", err, config.ErrSSHConnect)
	}
}
