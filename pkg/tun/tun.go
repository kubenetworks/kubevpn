package tun

import (
	"errors"
	"net"
	"os"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	log "github.com/sirupsen/logrus"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

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

	conn, ifce, err := createTun(config)
	if err != nil {
		log.Errorf("create tun device error: %v", err)
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
	ifce  tun.Device
	addr  net.Addr
	addr6 net.Addr
}

func (c *tunConn) Read(b []byte) (n int, err error) {
	offset := device.MessageTransportHeaderSize
	bytes := config.LPool.Get().([]byte)[:]
	defer config.LPool.Put(bytes[:])

	var num int
	sizes := make([]int, 1)
	num, err = c.ifce.Read([][]byte{bytes[:]}, sizes, offset)
	if err != nil {
		if errors.Is(err, tun.ErrTooManySegments) {
			log.Errorf("Dropped some packets from multi-segment read: %v", err)
			return 0, nil
		}
		if !errors.Is(err, os.ErrClosed) {
			log.Errorf("Failed to read packet from TUN device: %v", err)
			return 0, nil
		}
		return 0, err
	}
	if num == 0 {
		return 0, nil
	}
	var size = sizes[0]
	if size == 0 {
		return 0, nil
	}
	//if size > device.MaxSegmentSize-device.MessageTransportHeaderSize {
	//	return 0, errors.New("tun packet is too large")
	//}
	return copy(b, bytes[offset:offset+size]), nil
}

func (c *tunConn) Write(b []byte) (int, error) {
	if len(b) < device.MessageTransportHeaderSize {
		return 0, errors.New("tun packet is too short")
	}
	bytes := config.LPool.Get().([]byte)[:]
	defer config.LPool.Put(bytes[:])

	copy(bytes[device.MessageTransportOffsetContent:], b)

	_, err := c.ifce.Write([][]byte{bytes[:device.MessageTransportOffsetContent+len(b)]}, device.MessageTransportOffsetContent)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *tunConn) Close() error {
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
func AddRoutes(tunName string, routes ...types.Route) error {
	return addTunRoutes(tunName, routes...)
}
