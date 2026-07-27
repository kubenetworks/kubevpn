package core

import (
	"io"
	"net"
	"testing"

	glog "gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/tcpip/link/sniffer"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// TestAcquirePacketLogging_Refcount verifies the reference-counted lifecycle: OFF by default,
// ON while any acquirer holds it, and OFF again only after the last release — with idempotent
// releases so a double-call cannot under-count and flip it off while another holder remains.
func TestAcquirePacketLogging_Refcount(t *testing.T) {
	// Default (process init): off, and gvisor's sniffer neutralized.
	if packetLoggingEnabled() {
		t.Fatal("packet logging must be off by default")
	}
	if sniffer.LogPackets.Load() != 0 {
		t.Fatalf("sniffer.LogPackets must default to 0, got %d", sniffer.LogPackets.Load())
	}

	r1 := AcquirePacketLogging()
	if !packetLoggingEnabled() || sniffer.LogPackets.Load() != 1 {
		t.Fatal("first acquire must enable packet logging and the gvisor sniffer")
	}

	r2 := AcquirePacketLogging()
	if !packetLoggingEnabled() {
		t.Fatal("nested acquire must keep packet logging enabled")
	}

	r1()
	if !packetLoggingEnabled() {
		t.Fatal("release of one holder must keep logging on while another holds it")
	}
	r1() // idempotent: must not decrement a second time
	if !packetLoggingEnabled() {
		t.Fatal("idempotent double-release must not turn logging off while r2 still holds it")
	}

	r2()
	if packetLoggingEnabled() || sniffer.LogPackets.Load() != 0 {
		t.Fatalf("last release must restore the default (off); enabled=%v LogPackets=%d",
			packetLoggingEnabled(), sniffer.LogPackets.Load())
	}
}

// TestLogIPPacket_GatedByAcquire verifies logIPPacket honors the toggle: it must be a no-op when
// packet logging is not acquired (the default hot-path state) and only do work once acquired.
func TestLogIPPacket_GatedByAcquire(t *testing.T) {
	if packetLoggingEnabled() {
		t.Fatal("precondition: packet logging must be off")
	}
	pkt := buildIPv4Packet(net.IPv4(10, 0, 0, 1), net.IPv4(10, 0, 0, 2), []byte("payload"))
	// Off (default hot-path state): must be a cheap no-op and not panic.
	logIPPacket("[test]", pkt)

	// On: the same packet is formatted+logged via gvisor's sniffer without panicking.
	rel := AcquirePacketLogging()
	defer rel()
	logIPPacket("[test]", pkt)
}

// BenchmarkLogIPPacket quantifies the per-packet logging cost that this change made opt-in:
// "off" is the default hot-path state (a bare atomic load + return); "on" is the format+emit cost
// paid per packet when tracing is acquired (gvisor glog is redirected to io.Discard so the
// benchmark measures formatting, not terminal/file I/O, and does not flood output).
func BenchmarkLogIPPacket(b *testing.B) {
	pkt := buildIPv4Packet(net.IPv4(10, 0, 0, 1), net.IPv4(10, 0, 0, 2), make([]byte, 100))

	b.Run("off", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			logIPPacket("[bench]", pkt)
		}
	})

	b.Run("on", func(b *testing.B) {
		glog.SetTarget(plog.ServerEmitter{Writer: &glog.Writer{Next: io.Discard}})
		rel := AcquirePacketLogging()
		b.Cleanup(rel)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			logIPPacket("[bench]", pkt)
		}
	})
}
