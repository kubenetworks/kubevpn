package core

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// debugCtx returns a ctx carrying a Debug-level message-only logger writing to buf.
func debugCtx(buf *bytes.Buffer) context.Context {
	logger := plog.GetLoggerForClient(int32(log.DebugLevel), buf)
	return plog.WithLogger(context.Background(), logger)
}

// infoCtx returns a ctx carrying an Info-level message-only logger writing to buf. Used to
// assert that packet logging is suppressed when the logger itself is not at Debug (as in the
// CLI/server process without --debug); the daemon's file logger is always Debug.
func infoCtx(buf *bytes.Buffer) context.Context {
	logger := plog.GetLoggerForClient(int32(log.InfoLevel), buf)
	return plog.WithLogger(context.Background(), logger)
}

// TestClientLog_Outbound verifies the client logs every outbound packet (read
// from the local TUN) at Debug with an OUTBOUND tag when the ctx logger is at Debug.
func TestClientLog_Outbound(t *testing.T) {
	var buf bytes.Buffer
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

// TestClientLog_Inbound verifies the client logs every inbound packet (read from
// the server connection) at Debug with an INBOUND tag when the ctx logger is at Debug.
func TestClientLog_Inbound(t *testing.T) {
	var buf bytes.Buffer
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

// TestClientLog_InfoLoggerSuppressed verifies no per-packet line when the ctx logger is below
// Debug (e.g. CLI/server without --debug). Packet logging is gated on the logger level, not the
// global config.Debug flag — so the daemon (file logger always Debug) still records packets.
func TestClientLog_InfoLoggerSuppressed(t *testing.T) {
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(infoCtx(&buf))
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
		t.Fatalf("expected no packet line when debug disabled, got %q", buf.String())
	}
}
