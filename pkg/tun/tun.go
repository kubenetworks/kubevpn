package tun

import (
	"errors"
	"net"
	"os"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/pkg/core"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"

	"github.com/wencaiwulue/kubevpn/pkg/config"
)

// Config is the config for TUN device.
type Config struct {
	Name    string
	Addr    string
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

	conn, ifce, err := createTun(config)
	if err != nil {
		return nil, err
	}
	addrs, _ := ifce.Addrs()
	log.Debugf("[tun] %s: name: %s, mtu: %d, addrs: %s", conn.LocalAddr(), ifce.Name, ifce.MTU, addrs)

	ln.addr = conn.LocalAddr()
	ln.conns <- conn
	return ln, nil
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
	ifce tun.Device
	addr net.Addr
}

func (c *tunConn) Read(b []byte) (n int, err error) {
	offset := device.MessageTransportHeaderSize
	bytes := core.LPool.Get().([]byte)
	core.LPool.Put(bytes)

	size, err := c.ifce.Read(bytes[:], offset)
	if size == 0 || size > device.MaxSegmentSize-device.MessageTransportHeaderSize {
		return 0, nil
	}
	return copy(b, bytes[offset:offset+size]), nil
}

func (c *tunConn) Write(b []byte) (n int, err error) {
	if len(b) < device.MessageTransportHeaderSize {
		return 0, err
	}
	bytes := core.LPool.Get().([]byte)
	defer core.LPool.Put(bytes)

	copy(bytes[device.MessageTransportOffsetContent:], b)

	return c.ifce.Write(bytes[:device.MessageTransportOffsetContent+len(b)], device.MessageTransportOffsetContent)
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

func (c *tunConn) SetDeadline(time.Time) error {
	return &net.OpError{Op: "set", Net: "tun", Source: nil, Addr: nil, Err: errors.New("deadline not supported")}
}

func (c *tunConn) SetReadDeadline(time.Time) error {
	return &net.OpError{Op: "set", Net: "tun", Source: nil, Addr: nil, Err: errors.New("read deadline not supported")}
}

func (c *tunConn) SetWriteDeadline(time.Time) error {
	return &net.OpError{Op: "set", Net: "tun", Source: nil, Addr: nil, Err: errors.New("write deadline not supported")}
}

// AddRoutes for outer called
func AddRoutes(routes ...types.Route) error {
	env := os.Getenv(config.EnvTunNameOrLUID)
	return addTunRoutes(env, routes...)
}

func GetInterface() (*net.Interface, error) {
	return getInterface()
}
