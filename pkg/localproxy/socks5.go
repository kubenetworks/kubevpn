package localproxy

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
)

const (
	socks5Version       = 5
	socks5AuthNone      = 0
	socks5CmdConnect    = 1
	socks5AddrTypeIPv4  = 1
	socks5AddrTypeFQDN  = 3
	socks5AddrTypeIPv6  = 4
	socks5ReplySuccess  = 0
	socks5ReplyGeneral  = 1
	socks5ReplyNotAllow = 2
	socks5ReplyUnsup    = 7
)

func ServeSOCKS5(ctx context.Context, ln net.Listener, connector Connector) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}
		go handleSOCKS5Conn(ctx, conn, connector)
	}
}

func handleSOCKS5Conn(ctx context.Context, conn net.Conn, connector Connector) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	if err := socks5Handshake(reader, conn); err != nil {
		return
	}
	host, port, err := socks5ReadRequest(reader)
	if err != nil {
		_ = writeSOCKS5Reply(conn, socks5ReplyGeneral)
		return
	}

	remote, err := connector.Connect(ctx, host, port)
	if err != nil {
		_ = writeSOCKS5Reply(conn, socks5ReplyGeneral)
		return
	}
	defer remote.Close()

	if err := writeSOCKS5Reply(conn, socks5ReplySuccess); err != nil {
		return
	}
	relayConns(conn, remote)
}

func socks5Handshake(reader *bufio.Reader, conn net.Conn) error {
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return err
	}
	if header[0] != socks5Version {
		return fmt.Errorf("unsupported socks version %d", header[0])
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(reader, methods); err != nil {
		return err
	}
	_, err := conn.Write([]byte{socks5Version, socks5AuthNone})
	return err
}

func socks5ReadRequest(reader *bufio.Reader) (string, int, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return "", 0, err
	}
	if header[0] != socks5Version {
		return "", 0, fmt.Errorf("unsupported socks request version %d", header[0])
	}
	if header[1] != socks5CmdConnect {
		return "", 0, fmt.Errorf("unsupported socks command %d", header[1])
	}

	var host string
	switch header[3] {
	case socks5AddrTypeIPv4:
		addr := make([]byte, 4)
		if _, err := io.ReadFull(reader, addr); err != nil {
			return "", 0, err
		}
		host = net.IP(addr).String()
	case socks5AddrTypeIPv6:
		addr := make([]byte, 16)
		if _, err := io.ReadFull(reader, addr); err != nil {
			return "", 0, err
		}
		host = net.IP(addr).String()
	case socks5AddrTypeFQDN:
		length, err := reader.ReadByte()
		if err != nil {
			return "", 0, err
		}
		addr := make([]byte, int(length))
		if _, err := io.ReadFull(reader, addr); err != nil {
			return "", 0, err
		}
		host = string(addr)
	default:
		return "", 0, fmt.Errorf("unsupported socks address type %d", header[3])
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(reader, portBuf); err != nil {
		return "", 0, err
	}
	return host, int(binary.BigEndian.Uint16(portBuf)), nil
}

func writeSOCKS5Reply(conn net.Conn, rep byte) error {
	_, err := conn.Write([]byte{socks5Version, rep, 0, socks5AddrTypeIPv4, 0, 0, 0, 0, 0, 0})
	return err
}
