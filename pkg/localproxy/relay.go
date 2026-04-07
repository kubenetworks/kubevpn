package localproxy

import (
	"io"
	"net"
	"sync"
)

func relayConns(left, right net.Conn) {
	var wg sync.WaitGroup
	copyHalf := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if tcp, ok := dst.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}

	wg.Add(2)
	go copyHalf(left, right)
	go copyHalf(right, left)
	wg.Wait()
}
