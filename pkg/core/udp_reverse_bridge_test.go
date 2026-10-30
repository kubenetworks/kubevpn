package core

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// A real UDP endpoint whose read can be held after Close unblocks the socket.
// The relay must join the worker, not merely initiate transport shutdown.
type framingUDPEndpoint struct {
	*connectedUDP
	entered, release, exited chan struct{}
}

func (u *framingUDPEndpoint) readFrom(b []byte) (int, error) {
	close(u.entered)
	n, err := u.connectedUDP.readFrom(b)
	<-u.release
	close(u.exited)
	return n, err
}
func framingUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestFramingReverseJoinsWorkers(t *testing.T) {
	raw, peer := batchTCPPair(t, false)
	u := &framingUDPEndpoint{connectedUDP: &connectedUDP{framingUDP(t)}, entered: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{})}
	defer close(u.release)
	done := make(chan struct{})
	go func() { defer close(done); relayUDPReverse(context.Background(), raw, u) }()
	framingWait(t, u.entered)
	_ = peer.Close()
	// A short observation window is a negative assertion: the worker is held.
	select {
	case <-done:
		t.Error("relay returned before UDP worker released its buffer")
	case <-time.After(100 * time.Millisecond):
	}
	// Release via defer after checking; cleanup must not leave a worker behind.
	t.Cleanup(func() { framingWait(t, u.exited); framingWait(t, done) })
}

func TestFramingForwardCancellation(t *testing.T) {
	raw, peer := batchTCPPair(t, false)
	tracked := &framingConn{Conn: raw, readEntered: make(chan struct{}, 1)}
	u := framingUDP(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); relayUDPOverTCP(ctx, tracked, u) }()
	framingWait(t, tracked.readEntered)
	cancel()
	framingWait(t, done)
	if tracked.closes.Load() != 1 {
		t.Error("forward relay left TCP socket open")
	}
	_ = peer.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("peer read=%v want EOF", err)
	}
	_ = raw.Close()
}

func TestFramingReverseSetup(t *testing.T) {
	t.Run("cancelHeader", func(t *testing.T) {
		raw, _ := batchTCPPair(t, false)
		tracked := &framingConn{Conn: raw, readEntered: make(chan struct{}, 1)}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); _ = ServeUDPReverse(ctx, tracked) }()
		framingWait(t, tracked.readEntered)
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			_ = raw.Close()
			t.Error("setup header read ignored cancellation")
		}
		framingWait(t, done)
	})
	t.Run("cancelWriteHeader", func(t *testing.T) {
		raw, peer := batchTCPPair(t, false)
		framingSaturateSocket(t, raw, peer)
		tracked := &framingConn{Conn: raw, writeEntered: make(chan struct{}, 1)}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); _ = DialUDPReverse(ctx, tracked, 12345, 12346) }()
		framingWait(t, tracked.writeEntered)
		cancel()
		framingWait(t, done)
	})
	t.Run("shortHeader", func(t *testing.T) {
		raw, peer := batchTCPPair(t, false)
		tracked := &framingConn{Conn: raw, writeLimit: 1}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); _ = DialUDPReverse(ctx, tracked, 12345, 12346) }()
		_ = peer.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		var hdr [2]byte
		if _, err := io.ReadFull(peer, hdr[:]); err != nil {
			t.Errorf("setup header truncated: %v", err)
		} else if binary.BigEndian.Uint16(hdr[:]) != 12345 {
			t.Error("wrong port")
		}
		cancel()
		_ = peer.Close()
		framingWait(t, done)
	})
}

func TestFramingUDPRelayEcho(t *testing.T) {
	for _, tls := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("tls=%v/reverse=%v", tls, reverse), func(t *testing.T) {
				echo := framingUDP(t)
				echoDone := make(chan struct{})
				go func() {
					defer close(echoDone)
					b := make([]byte, 2048)
					for {
						n, a, e := echo.ReadFromUDP(b)
						if e != nil {
							return
						}
						_, _ = echo.WriteToUDP(b[:n], a)
					}
				}()
				defer func() { _ = echo.Close(); framingWait(t, echoDone) }()
				u, err := net.DialUDP("udp4", nil, echo.LocalAddr().(*net.UDPAddr))
				if err != nil {
					t.Fatal(err)
				}
				raw, peer := batchTCPPair(t, tls)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan struct{})
				go func() {
					defer close(done)
					if reverse {
						relayUDPReverse(ctx, &framingConn{Conn: raw, writeLimit: 1}, &connectedUDP{u})
					} else {
						relayUDPOverTCP(ctx, &framingConn{Conn: raw, writeLimit: 1}, u)
					}
				}()
				c, _ := NewUDPConnOverTCP(ctx, peer)
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(3 * time.Second))
				for _, payload := range [][]byte{[]byte("ping"), bytes.Repeat([]byte{9}, 1024)} {
					if _, err := c.Write(payload); err != nil {
						t.Fatal(err)
					}
					b := make([]byte, len(payload))
					n, err := c.Read(b)
					if err != nil || n != len(payload) || !bytes.Equal(b, payload) {
						t.Fatalf("echo n=%d err=%v", n, err)
					}
				}
				cancel()
				framingWait(t, done)
			})
		}
	}
}

// TestUDPReverseBridge_EndToEnd wires the pod side (ServeUDPReverse) and the dev
// side (DialUDPReverse) over an in-memory net.Pipe, with a loopback UDP echo
// server standing in for the developer's local service. It then sends UDP to the
// "envoy" port and asserts the datagram is bridged to the echo server and back —
// the full fargate UDP inbound path minus the cluster.
func TestUDPReverseBridge_EndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Local UDP echo server (stands in for the dev's udpServer).
	echo, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	localPort := echo.LocalAddr().(*net.UDPAddr).Port
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := echo.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = echo.WriteToUDP(buf[:n], addr)
		}
	}()

	// Pick a free UDP port for the "envoy" side that ServeUDPReverse will bind.
	tmp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	envoyPort := tmp.LocalAddr().(*net.UDPAddr).Port
	_ = tmp.Close()

	podSide, devSide := net.Pipe()
	defer podSide.Close()
	defer devSide.Close()
	go func() { _ = ServeUDPReverse(ctx, podSide) }()
	go func() { _ = DialUDPReverse(ctx, devSide, envoyPort, localPort) }()

	// "envoy" client dials the bridged UDP port and expects an echo.
	client, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: envoyPort})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var got string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_ = client.SetDeadline(time.Now().Add(200 * time.Millisecond))
		if _, err = client.Write([]byte("ping")); err != nil {
			continue
		}
		buf := make([]byte, 64)
		n, rerr := client.Read(buf)
		if rerr == nil {
			got = string(buf[:n])
			break
		}
	}
	if got != "ping" {
		t.Fatalf("reverse UDP bridge did not echo: got %q want %q", got, "ping")
	}
}
