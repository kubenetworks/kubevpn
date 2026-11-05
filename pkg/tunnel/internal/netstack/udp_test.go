package netstack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// udpLogBuffer is a concurrency-safe log sink for capturing plog output in tests.
type udpLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *udpLogBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *udpLogBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func udpWaitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// udpTestPacket builds a valid IPv4/UDP packet whose destination port selects the
// forwarder's dial target; the destination address is arbitrary (LocalUDPForwarder
// always resolves to loopback).
func udpTestPacket(dstPort uint16, payload int) []byte {
	p := make([]byte, 28+payload)
	header.IPv4(p).Encode(&header.IPv4Fields{
		TotalLength: uint16(len(p)), TTL: 64, Protocol: uint8(udp.ProtocolNumber),
		SrcAddr: tcpip.AddrFrom4([4]byte{198, 18, 0, 1}), DstAddr: tcpip.AddrFrom4([4]byte{198, 18, 0, 2}),
	})
	header.IPv4(p).SetChecksum(^header.IPv4(p).CalculateChecksum())
	header.UDP(p[20:]).Encode(&header.UDPFields{SrcPort: 1234, DstPort: dstPort, Length: uint16(8 + payload)})
	return p
}

// TestUDPForwarderIdleTimeoutIsQuiet wires a full LocalUDPForwarder stack, forwards one
// UDP packet to a real loopback listener, and lets the session expire. An idle-expiry
// teardown is normal lifecycle, so it must log at debug level ("Idle session closed"),
// not error level ("Disconnected").
func TestUDPForwarderIdleTimeoutIsQuiet(t *testing.T) {
	orig := config.UDPSessionTimeout
	config.UDPSessionTimeout = 150 * time.Millisecond
	t.Cleanup(func() { config.UDPSessionTimeout = orig })

	logs := &udpLogBuffer{}
	ctx, cancel := context.WithCancel(plog.WithLogger(context.Background(),
		plog.GetLoggerForClient(int32(logrus.DebugLevel), logs)))
	defer cancel()

	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	dstPort := uint16(listener.LocalAddr().(*net.UDPAddr).Port)

	ep := channel.New(256, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	s := NewLocalStack(ctx, ep)
	t.Cleanup(func() { s.Destroy(); ep.Close() })

	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(udpTestPacket(dstPort, 8))})
	ep.InjectInbound(header.IPv4ProtocolNumber, pkt)
	pkt.DecRef()

	udpWaitFor(t, 5*time.Second, func() bool {
		return strings.Contains(logs.String(), "Idle session closed")
	}, "debug-level idle session close after read deadline expiry")

	if out := logs.String(); strings.Contains(out, "Disconnected") {
		t.Errorf("idle expiry misclassified as a real failure; logs: %q", out)
	}
}

// TestRelayUDPRealErrorStaysError verifies a non-timeout relay failure (here: operations
// on an already-closed remote socket return net.ErrClosed) still logs at error level.
func TestRelayUDPRealErrorStaysError(t *testing.T) {
	logs := &udpLogBuffer{}
	ctx, cancel := context.WithCancel(plog.WithLogger(context.Background(),
		plog.GetLoggerForClient(int32(logrus.DebugLevel), logs)))
	defer cancel()

	ep := channel.New(256, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	s := NewLocalStack(ctx, ep)
	defer func() { s.Destroy(); ep.Close() }()

	wq := &waiter.Queue{}
	endpoint, err := s.NewEndpoint(udp.ProtocolNumber, header.IPv4ProtocolNumber, wq)
	if err != nil {
		t.Fatal(err)
	}
	conn := gonet.NewUDPConn(wq, endpoint)

	remote, dialErr := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9})
	if dialErr != nil {
		t.Fatal(err)
	}
	// Closing before the relay runs makes pipeUDPToConn fail at SetReadDeadline
	// immediately with net.ErrClosed — a real failure, never an idle deadline.
	remote.Close()

	src := &net.UDPAddr{IP: net.ParseIP("198.18.0.1"), Port: 1234}
	dst := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9}
	relayUDP(ctx, conn, remote, src, dst)

	out := logs.String()
	if !strings.Contains(out, "Disconnected") {
		t.Fatalf("expected error-level Disconnected log for a real failure, got: %q", out)
	}
	if strings.Contains(out, "Idle session closed") {
		t.Fatalf("real failure misclassified as idle timeout: %q", out)
	}
}

type fakeTimeoutError struct{ timeout bool }

func (fakeTimeoutError) Error() string   { return "fake net error" }
func (f fakeTimeoutError) Timeout() bool { return f.timeout }
func (fakeTimeoutError) Temporary() bool { return false }

func TestIsIdleTimeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"deadline exceeded", os.ErrDeadlineExceeded, true},
		{"wrapped deadline exceeded", fmt.Errorf("read udp: %w", os.ErrDeadlineExceeded), true},
		{"net error with timeout", fakeTimeoutError{timeout: true}, true},
		{"net error without timeout", fakeTimeoutError{timeout: false}, false},
		{"closed socket", &net.OpError{Op: "read", Err: net.ErrClosed}, false},
		{"generic error", errors.New("connection reset"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isIdleTimeout(tc.err); got != tc.want {
				t.Errorf("isIdleTimeout(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}
