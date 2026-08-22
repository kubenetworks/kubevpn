package tun

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/containernetworking/cni/pkg/types"
)

// ErrClosed is returned by Accept when the TUN listener has been closed.
var ErrClosed = errors.New("accept on closed listener")

// Config is the config for TUN device.
type Config struct {
	Name    string
	Addr    string
	Addr6   string
	MTU     int
	Routes  []types.Route
	Gateway string
}

type tunListener struct {
	addr   net.Addr
	conns  chan net.Conn
	closed chan struct{}
	config Config
}

// Listener TunListener creates a listener for tun tunnel.
func Listener(config Config) (net.Listener, error) {
	ln := &tunListener{
		conns:  make(chan net.Conn, 1),
		closed: make(chan struct{}),
		config: config,
	}

	conn, _, err := createTun(config)
	if err != nil {
		err = fmt.Errorf("create tun device failed: %w", err)
		return nil, err
	}

	ln.addr = conn.LocalAddr()
	ln.conns <- conn
	return ln, nil
}

func (l *tunListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, ErrClosed
	}
}

func (l *tunListener) Addr() net.Addr {
	return l.addr
}

func (l *tunListener) Close() error {
	select {
	case <-l.closed:
		return errors.New("listener has been closed")
	default:
		close(l.closed)
	}
	return nil
}

type tunConn struct {
	*batchDevice
	addr  net.Addr
	addr6 net.Addr
}

func (c *tunConn) Close() (err error) {
	return c.batchDevice.Close()
}

func (c *tunConn) LocalAddr() net.Addr {
	return c.addr
}

func (c *tunConn) RemoteAddr() net.Addr {
	return &net.IPAddr{}
}

func (c *tunConn) SetDeadline(time.Time) error {
	return &net.OpError{Op: "set", Net: "tun", Source: nil, Addr: nil, Err: errors.New("deadline not supported")}
}

func (c *tunConn) SetReadDeadline(time.Time) error {
	return &net.OpError{Op: "set", Net: "tun", Source: nil, Addr: nil, Err: errors.New("read deadline not supported")}
}

func (c *tunConn) SetWriteDeadline(time.Time) error {
	return &net.OpError{Op: "set", Net: "tun", Source: nil, Addr: nil, Err: errors.New("write deadline not supported")}
}
