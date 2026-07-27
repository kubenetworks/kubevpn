package core

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	glog "gvisor.dev/gvisor/pkg/log"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// debugCtx returns a ctx carrying a Debug-level message-only logger writing to buf.
func debugCtx(buf *bytes.Buffer) context.Context {
	logger := plog.GetLoggerForClient(int32(log.DebugLevel), buf)
	return plog.WithLogger(context.Background(), logger)
}

// bufferEmitter captures gvisor glog output into a bytes.Buffer for test assertions.
type bufferEmitter struct {
	buf *bytes.Buffer
}

func (e *bufferEmitter) Emit(_ int, _ glog.Level, _ time.Time, format string, args ...any) {
	fmt.Fprintf(e.buf, format, args...)
	e.buf.WriteByte('\n')
}

// setGvisorLog redirects gvisor glog to buf and returns a cleanup function that restores the
// previous target. Not safe for parallel tests (gvisor glog is global).
func setGvisorLog(buf *bytes.Buffer) func() {
	old := glog.Log()
	glog.SetTarget(&bufferEmitter{buf: buf})
	return func() {
		glog.SetTarget(old)
	}
}

// TestClientLog_Outbound verifies that, WHILE packet tracing is acquired, the client logs every
// outbound packet (read from the local TUN) with an OUTBOUND tag and the flow addresses.
func TestClientLog_Outbound(t *testing.T) {
	var buf bytes.Buffer
	cleanup := setGvisorLog(&buf)
	defer cleanup()
	rel := AcquirePacketLogging()
	defer rel()

	ctx, cancel := context.WithCancel(debugCtx(&buf))
	defer cancel()

	tun := newMockTUN()
	device := &tunDevice{
		tun:         tun,
		tunInbound:  make(chan *Packet, MaxSize),
		tunOutbound: make(chan *Packet, MaxSize),
		errChan:     make(chan error, 1),
	}
	device.transport = newClientTransport(device, nil, nil)
	go device.readFromTun(ctx)

	tun.readCh <- buildIPv4Packet(net.IPv4(198, 18, 0, 2), net.IPv4(10, 0, 0, 5), []byte("hi"))

	select {
	case pkt := <-device.tunInbound: // receive happens-after the log write
		config.LPool.Put(pkt.data[:])
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for outbound packet")
	}

	line := buf.String()
	for _, want := range []string{"OUTBOUND", "198.18.0.2", "10.0.0.5"} {
		if !strings.Contains(line, want) {
			t.Fatalf("outbound log %q missing %q", line, want)
		}
	}
}

// TestClientLog_Inbound verifies that, WHILE packet tracing is acquired, the client logs every
// inbound packet (read from the server connection) with an INBOUND tag and the flow addresses.
func TestClientLog_Inbound(t *testing.T) {
	var buf bytes.Buffer
	cleanup := setGvisorLog(&buf)
	defer cleanup()
	rel := AcquirePacketLogging()
	defer rel()

	ctx, cancel := context.WithCancel(debugCtx(&buf))
	defer cancel()

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	tunOutbound := make(chan *Packet, MaxSize)
	errChan := make(chan error, 2)
	slot := &connSlot{
		id:          0,
		inbound:     make(chan *Packet, MaxSize),
		tunOutbound: tunOutbound,
	}
	go slot.readFromConn(ctx, client, errChan)

	// Inbound wire format: [1-byte prefix][IP packet]. prefix 0 → tunOutbound path.
	ipPkt := buildIPv4Packet(net.IPv4(10, 0, 0, 5), net.IPv4(198, 18, 0, 2), []byte("pong"))
	frame := append([]byte{0}, ipPkt...)
	go func() { _, _ = server.Write(frame) }()

	select {
	case pkt := <-tunOutbound: // receive happens-after the log write
		config.LPool.Put(pkt.data[:])
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for inbound packet")
	}

	line := buf.String()
	for _, want := range []string{"INBOUND", "10.0.0.5", "198.18.0.2"} {
		if !strings.Contains(line, want) {
			t.Fatalf("inbound log %q missing %q", line, want)
		}
	}
}

// TestClientLog_SuppressedByDefault verifies that per-packet logging is off unless explicitly
// acquired: even with a Debug-level ctx logger (which used to enable it), no packet line is
// emitted. Per-packet tracing is now decoupled from the logger level — it is an explicit,
// reference-counted opt-in (AcquirePacketLogging), so it never floods the daemon log by default.
func TestClientLog_SuppressedByDefault(t *testing.T) {
	if packetLoggingEnabled() {
		t.Fatal("precondition: packet logging must be off (do not acquire in this test)")
	}
	var buf bytes.Buffer
	cleanup := setGvisorLog(&buf)
	defer cleanup()

	ctx, cancel := context.WithCancel(debugCtx(&buf))
	defer cancel()

	tun := newMockTUN()
	device := &tunDevice{
		tun:         tun,
		tunInbound:  make(chan *Packet, MaxSize),
		tunOutbound: make(chan *Packet, MaxSize),
		errChan:     make(chan error, 1),
	}
	device.transport = newClientTransport(device, nil, nil)
	go device.readFromTun(ctx)
	tun.readCh <- buildIPv4Packet(net.IPv4(198, 18, 0, 2), net.IPv4(10, 0, 0, 5), []byte("hi"))

	select {
	case pkt := <-device.tunInbound:
		config.LPool.Put(pkt.data[:])
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for packet")
	}

	if strings.Contains(buf.String(), "OUTBOUND") {
		t.Fatalf("expected no packet line when tracing not acquired, got %q", buf.String())
	}
}
