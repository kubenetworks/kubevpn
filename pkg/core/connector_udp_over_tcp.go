package core

import (
	"context"
	"net"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// UDPOverTCPConnector wraps a TCP connection as a UDP-like datagram connection.
type UDPOverTCPConnector struct {
}

// NewUDPOverTCPConnector returns a Connector that wraps TCP with datagram framing.
func NewUDPOverTCPConnector() Connector {
	return &UDPOverTCPConnector{}
}

func (c *UDPOverTCPConnector) ConnectContext(ctx context.Context, conn net.Conn) (net.Conn, error) {
	//defer conn.SetDeadline(time.Time{})
	switch con := conn.(type) {
	case *net.TCPConn:
		err := con.SetNoDelay(true)
		if err != nil {
			return nil, err
		}
		err = con.SetKeepAlive(true)
		if err != nil {
			return nil, err
		}
		err = con.SetKeepAlivePeriod(config.KeepAliveTime)
		if err != nil {
			return nil, err
		}
	}
	return NewUDPConnOverTCP(ctx, conn)
}
