package localproxy

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
)

func ServeHTTPConnect(ctx context.Context, ln net.Listener, connector Connector) error {
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
		go handleHTTPConnectConn(ctx, conn, connector)
	}
}

func handleHTTPConnectConn(ctx context.Context, conn net.Conn, connector Connector) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		_, _ = fmt.Fprint(conn, "HTTP/1.1 405 Method Not Allowed\r\nConnection: close\r\n\r\n")
		return
	}

	host, portStr, err := net.SplitHostPort(req.Host)
	if err != nil {
		_, _ = fmt.Fprint(conn, "HTTP/1.1 400 Bad Request\r\nConnection: close\r\n\r\n")
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		_, _ = fmt.Fprint(conn, "HTTP/1.1 400 Bad Request\r\nConnection: close\r\n\r\n")
		return
	}

	remote, err := connector.Connect(ctx, host, port)
	if err != nil {
		_, _ = fmt.Fprint(conn, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")
		return
	}
	defer remote.Close()

	_, _ = fmt.Fprint(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
	relayConns(conn, remote)
}
