package core

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// Reverse-UDP bridge.
//
// Fargate/Service mode has no NET_ADMIN/iptables and the SSH reverse tunnel is
// TCP-only, so envoy's UDP inbound (forwarded to 127.0.0.1:envoyPort in the pod)
// cannot reach the developer. This bridge carries UDP over a single TCP control
// connection that the developer opens directly to the sidecar (reachable via the
// VPN), reusing the length-prefixed UDPConnOverTCP framing.
//
//	envoy --udp--> 127.0.0.1:envoyPort (pod)
//	             ServeUDPReverse: UDP listen -> frame over ctrl conn ----+
//	                                                                     | TCP (UDPConnOverTCP)
//	             DialUDPReverse: read frames -> UDP dial 127.0.0.1:local +--> dev udpServer
//
// Wire format: the dev side first writes the 2-byte big-endian target UDP port
// (envoyPort) so the pod knows which port to listen on; the rest of the stream
// is UDPConnOverTCP datagrams.

// udpBridgeHandler serves the pod side of the reverse-UDP bridge: each accepted
// control connection drives one ServeUDPReverse.
type udpBridgeHandler struct{}

// UDPBridgeHandler creates a Handler for the reverse-UDP bridge listener.
func UDPBridgeHandler() Handler { return &udpBridgeHandler{} }

func (h *udpBridgeHandler) Handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	if err := ServeUDPReverse(ctx, conn); err != nil && !isClosedOrEOF(err) {
		plog.G(ctx).Debugf("[UDP-Bridge] serve: %v", err)
	}
}

// ServeUDPReverse runs the pod side of the bridge for one control connection: it
// reads the 2-byte target port, listens UDP on 127.0.0.1:port, and relays
// datagrams to/from the developer over conn. Blocks until conn or ctx closes.
func ServeUDPReverse(ctx context.Context, conn net.Conn) error {
	var hdr [2]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return fmt.Errorf("read udp-bridge port header: %w", err)
	}
	port := int(binary.BigEndian.Uint16(hdr[:]))

	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		return fmt.Errorf("udp-bridge listen 127.0.0.1:%d: %w", port, err)
	}
	defer udpConn.Close()
	plog.G(ctx).Debugf("[UDP-Bridge] serving udp 127.0.0.1:%d", port)

	relayUDPReverse(ctx, conn, &listenUDP{c: udpConn})
	return nil
}

// DialUDPReverse runs the developer side of the bridge over an established
// control connection: it announces envoyPort, then relays framed datagrams
// to/from the local UDP service at 127.0.0.1:localPort. Blocks until done.
func DialUDPReverse(ctx context.Context, conn net.Conn, envoyPort, localPort int) error {
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(envoyPort))
	if _, err := conn.Write(hdr[:]); err != nil {
		return fmt.Errorf("write udp-bridge port header: %w", err)
	}

	localConn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: localPort})
	if err != nil {
		return fmt.Errorf("udp-bridge dial 127.0.0.1:%d: %w", localPort, err)
	}
	defer localConn.Close()

	// On the dev side the local UDP socket is connected, so a plain *net.UDPConn
	// (no per-packet source tracking) is enough; wrap it to share relay logic.
	relayUDPReverse(ctx, conn, &connectedUDP{localConn})
	return nil
}

// udpEndpoint abstracts the per-packet UDP side of the relay so the pod side
// (unconnected listener, must reply to the last source) and the dev side
// (connected socket) can share one relay loop.
type udpEndpoint interface {
	readFrom(b []byte) (int, error)
	writeTo(b []byte) (int, error)
	Close() error
}

// connectedUDP wraps a connected *net.UDPConn.
type connectedUDP struct{ c *net.UDPConn }

func (u *connectedUDP) readFrom(b []byte) (int, error) { return u.c.Read(b) }
func (u *connectedUDP) writeTo(b []byte) (int, error)  { return u.c.Write(b) }
func (u *connectedUDP) Close() error                   { return u.c.Close() }

// listenUDP wraps an unconnected listener, remembering the last peer to reply to.
type listenUDP struct {
	c    *net.UDPConn
	mu   sync.Mutex
	peer *net.UDPAddr
}

func (u *listenUDP) readFrom(b []byte) (int, error) {
	n, addr, err := u.c.ReadFromUDP(b)
	if addr != nil {
		u.mu.Lock()
		u.peer = addr
		u.mu.Unlock()
	}
	return n, err
}

func (u *listenUDP) writeTo(b []byte) (int, error) {
	u.mu.Lock()
	peer := u.peer
	u.mu.Unlock()
	if peer == nil {
		return len(b), nil // no peer seen yet; drop
	}
	return u.c.WriteToUDP(b, peer)
}

func (u *listenUDP) Close() error { return u.c.Close() }

// relayUDPReverse pumps datagrams between the length-prefixed TCP control stream
// (conn) and a UDP endpoint in both directions until either side errors or ctx is
// done. Framing is done inline (writeDatagram / readDatagramPacket) so the UDP->TCP
// direction is zero-copy: UDP is read into the reserved headroom and the length
// header is stamped in place.
func relayUDPReverse(ctx context.Context, conn net.Conn, udpEnd udpEndpoint) {
	errChan := make(chan error, 2)
	// UDP -> conn (to the other side)
	go func() {
		buf := config.LPool.Get().([]byte)
		defer config.LPool.Put(buf)
		for {
			n, err := udpEnd.readFrom(buf[datagramHeaderLen:])
			if err != nil {
				errChan <- err
				return
			}
			if err = writeDatagram(conn, buf, n); err != nil {
				errChan <- err
				return
			}
		}
	}()
	// conn -> UDP (to the local/remote service)
	go func() {
		buf := config.LPool.Get().([]byte)
		defer config.LPool.Put(buf)
		for {
			dgram, err := readDatagramPacket(conn, buf)
			if err != nil {
				errChan <- err
				return
			}
			if _, err = udpEnd.writeTo(dgram.Data[:dgram.DataLength]); err != nil {
				errChan <- err
				return
			}
		}
	}()
	select {
	case err := <-errChan:
		if err != nil && !isClosedOrEOF(err) {
			plog.G(ctx).Debugf("[UDP-Bridge] relay ended: %v", err)
		}
	case <-ctx.Done():
	}
	_ = udpEnd.Close()
	_ = conn.Close()
}

func isClosedOrEOF(err error) bool {
	// Use errors.Is so wrapped errors (ServeUDPReverse wraps with %w) are still
	// recognized as a normal connection close rather than a real failure.
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) || errIsNetClosed(err)
}

func errIsNetClosed(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && !ne.Timeout()
}
