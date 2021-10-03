package tun

import (
	"errors"
	log "github.com/sirupsen/logrus"
	"github.com/songgao/water"
	"net"
	"os"
	"time"
)

// TunConfig is the config for TUN device.
type TunConfig struct {
	Name    string
	Addr    string
	Peer    string // peer addr of point-to-point on MacOS
	MTU     int
	Routes  []IPRoute
	Gateway string
}

type tunListener struct {
	addr   net.Addr
	conns  chan net.Conn
	closed chan struct{}
	config TunConfig
}

// TunListener creates a listener for tun tunnel.
func TunListener(cfg TunConfig) (Listener, error) {
	threads := 1
	ln := &tunListener{
		conns:  make(chan net.Conn, threads),
		closed: make(chan struct{}),
		config: cfg,
	}

	for i := 0; i < threads; i++ {
		conn, ifce, err := createTun(cfg)
		if err != nil {
			return nil, err
		}
		ln.addr = conn.LocalAddr()

		addrs, _ := ifce.Addrs()
		_ = os.Setenv("tunName", ifce.Name)
		log.Debugf("[tun] %s: name: %s, mtu: %d, addrs: %s",
			conn.LocalAddr(), ifce.Name, ifce.MTU, addrs)

		ln.conns <- conn
	}

	return ln, nil
}

// Listener is a proxy server listener, just like a net.Listener.
type Listener interface {
	net.Listener
}

func (l *tunListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
	}

	return nil, errors.New("accept on closed listener")
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
	ifce *water.Interface
	addr net.Addr
}

func (c *tunConn) Read(b []byte) (n int, err error) {
	return c.ifce.Read(b)
}

func (c *tunConn) Write(b []byte) (n int, err error) {
	return c.ifce.Write(b)
}

func (c *tunConn) Close() (err error) {
	return c.ifce.Close()
}

func (c *tunConn) LocalAddr() net.Addr {
	return c.addr
}

func (c *tunConn) RemoteAddr() net.Addr {
	return &net.IPAddr{}
}

func (c *tunConn) SetDeadline(t time.Time) error {
	return &net.OpError{Op: "set", Net: "tun", Source: nil, Addr: nil, Err: errors.New("deadline not supported")}
}

func (c *tunConn) SetReadDeadline(t time.Time) error {
	return &net.OpError{Op: "set", Net: "tun", Source: nil, Addr: nil, Err: errors.New("deadline not supported")}
}

func (c *tunConn) SetWriteDeadline(t time.Time) error {
	return &net.OpError{Op: "set", Net: "tun", Source: nil, Addr: nil, Err: errors.New("deadline not supported")}
}

// IPRoute is an IP routing entry.
type IPRoute struct {
	Dest    *net.IPNet
	Gateway net.IP
}
