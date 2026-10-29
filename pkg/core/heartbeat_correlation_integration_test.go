package core

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// A real gVisor echo server and framed reader must not mistake an application's
// gateway ping for a heartbeat. The marker packet also acts as a reader barrier.
func TestHeartbeatIntegration_UnsolicitedGatewayEcho(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ep := channel.New(16, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	s := NewStack(ctx, ep)
	defer s.Destroy()
	for _, v6 := range []bool{false, true} {
		var req *stack.PacketBuffer
		proto := header.IPv4ProtocolNumber
		if v6 {
			proto = header.IPv6ProtocolNumber
			req = buildICMPv6EchoRequest(tcpip.AddrFromSlice(net.ParseIP(regTestTunIPv6).To16()), tcpip.AddrFromSlice(config.RouterIP6.To16()), 7, 9, []byte("application ping"))
		} else {
			req = buildICMPv4EchoRequest(tcpip.AddrFromSlice(net.ParseIP(regTestTunIPv4).To4()), tcpip.AddrFromSlice(config.RouterIP.To4()), 7, 9, []byte("application ping"))
		}
		reply := injectEchoAndReadReply(t, ctx, ep, proto, req)
		if reply == nil {
			t.Fatal("gVisor did not echo application payload")
		}
		view := stack.PayloadSince(reply.NetworkHeader())
		ip := bytes.Clone(view.AsSlice())
		view.Release()
		reply.DecRef()
		stats := &HeartbeatStats{}
		out := make(chan *Packet, 4)
		client, server := net.Pipe()
		framed, err := NewUDPConnOverTCP(ctx, client)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			(&connSlot{id: 0, stats: stats, tunOutbound: out}).readFromConn(ctx, framed, errChanOf())
		}()
		if _, err := server.Write(frameDatagram(0, ip)); err != nil {
			t.Fatal(err)
		}
		marker := buildIPv4Packet(net.IPv4(10, 0, 0, 5), net.ParseIP(regTestTunIPv4), []byte("barrier"))
		if _, err := server.Write(frameDatagram(0, marker)); err != nil {
			t.Fatal(err)
		}
		client.Close()
		server.Close()
		<-done
		if !stats.LastReply().IsZero() {
			t.Errorf("IPv6=%v: unsolicited gateway echo renewed heartbeat health", v6)
		}
		if len(out) != 2 {
			t.Errorf("IPv6=%v: got %d business packets, want ping and marker", v6, len(out))
		}
		for len(out) > 0 {
			(<-out).release()
		}
	}
}

// probeWire joins the actual slot writer, framed pipe, gVisor echo server and
// reader. Replies are held by the test to model reordering and transport changes.
type probeWire struct {
	t          *testing.T
	ctx        context.Context
	stats      *HeartbeatStats
	writer     *connSlot
	in         chan *Packet
	requests   chan []byte
	server     net.Conn
	out        chan *Packet
	ep         *channel.Endpoint
	generation uint64
	writeCount uint64
}

func newProbeWire(t *testing.T, stats *HeartbeatStats) *probeWire {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	client, server := net.Pipe()
	framedClient, _ := NewUDPConnOverTCP(ctx, client)
	framedServer, _ := NewUDPConnOverTCP(ctx, server)
	ep := channel.New(16, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	s := NewStack(ctx, ep)
	w := &probeWire{t: t, ctx: ctx, stats: stats, in: make(chan *Packet, 256), requests: make(chan []byte), server: framedServer, out: make(chan *Packet, 4), ep: ep, generation: stats.Generation()}
	w.writeCount = stats.Snapshot().Slots[ConnPoolSize].WritePackets
	w.writer = &connSlot{id: ConnPoolSize, isControl: true, stats: stats, inbound: w.in}
	reader := &connSlot{id: 2, stats: stats, tunOutbound: w.out}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); w.writer.writeGeneration(ctx, client, errChanOf(), w.generation) }()
	go func() { defer wg.Done(); reader.readGeneration(ctx, framedClient, errChanOf(), w.generation) }()
	go func() {
		defer wg.Done()
		buf := make([]byte, config.LargeBufferSize)
		for {
			n, err := framedServer.Read(buf)
			if err != nil {
				return
			}
			select {
			case w.requests <- bytes.Clone(buf[:n]):
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		client.Close()
		server.Close()
		wg.Wait()
		s.Destroy()
		for len(w.in) > 0 {
			(<-w.in).release()
		}
		for len(w.out) > 0 {
			(<-w.out).release()
		}
	})
	return w
}

func (w *probeWire) send(v6 bool) []byte {
	w.t.Helper()
	dev := &tunDevice{addrsFn: func() (net.IP, net.IP, net.IP) {
		if v6 {
			return nil, net.ParseIP(regTestTunIPv6), nil
		}
		return net.ParseIP(regTestTunIPv4), nil, nil
	}}
	payload := (&clientTransport{dev: dev}).registrationPayloads(w.ctx)[0]
	buf := config.LPool.Get().([]byte)
	copy(buf[datagramHeaderLen:], payload)
	buf[datagramHeaderLen] = packetTypeControl
	w.in <- NewPacket(buf, len(payload), nil, nil)
	var got []byte
	select {
	case got = <-w.requests:
	case <-time.After(2 * time.Second):
		w.t.Fatal("probe was not written")
	}
	w.writeCount++
	waitFor(w.t, 2*time.Second, func() bool { return w.stats.Snapshot().Slots[ConnPoolSize].WritePackets >= w.writeCount }, "slot write completion")
	return got[typePrefixLen:]
}

func (w *probeWire) echo(ip []byte) []byte {
	w.t.Helper()
	proto := header.IPv4ProtocolNumber
	if ip[0]>>4 == 6 {
		proto = header.IPv6ProtocolNumber
	}
	req := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(bytes.Clone(ip))})
	reply := injectEchoAndReadReply(w.t, w.ctx, w.ep, proto, req)
	if reply == nil {
		w.t.Fatal("gVisor did not echo correlated probe")
	}
	view := stack.PayloadSince(reply.NetworkHeader())
	result := bytes.Clone(view.AsSlice())
	view.Release()
	reply.DecRef()
	off, roff := echoOffset(ip), echoOffset(result)
	if off < 0 || roff < 0 || !bytes.Equal(ip[off+4:], result[roff+4:]) {
		w.t.Fatal("gVisor changed echo identity or payload")
	}
	return result
}

func (w *probeWire) deliver(reply []byte) {
	w.t.Helper()
	w.server.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := w.server.Write(append([]byte{0}, reply...)); err != nil {
		w.t.Fatal(err)
	}
	marker := buildIPv4Packet(net.IPv4(10, 0, 0, 1), net.ParseIP(regTestTunIPv4), []byte("reader barrier"))
	if _, err := w.server.Write(append([]byte{0}, marker...)); err != nil {
		w.t.Fatal(err)
	}
	select {
	case pkt := <-w.out:
		defer pkt.release()
		if !bytes.Equal(pkt.data[tunReserve:datagramHeaderLen+pkt.length], marker) {
			w.t.Fatal("protocol echo leaked to business TUN")
		}
	case <-time.After(2 * time.Second):
		w.t.Fatal("business packet swallowed")
	}
}

func TestHeartbeatIntegration_CorrelationLifecycle(t *testing.T) {
	var elapsed atomic.Int64
	base := time.Now()
	stats := &HeartbeatStats{now: func() time.Time { return base.Add(time.Duration(elapsed.Load())) }}
	wire := newProbeWire(t, stats)
	for _, v6 := range []bool{false, true} {
		request := wire.send(v6)
		reply := wire.echo(request)
		elapsed.Add(int64(7 * time.Millisecond))
		wire.deliver(reply)
		first := stats.Snapshot()
		if first.LastRTT != 7*time.Millisecond || first.Pending != 0 {
			t.Fatalf("bad correlation: %+v", first)
		}
		elapsed.Add(int64(time.Second))
		wire.deliver(reply)
		after := stats.Snapshot()
		if !after.LastReply.Equal(first.LastReply) || after.Replies != first.Replies || after.Rejected != first.Rejected+1 {
			t.Fatal("duplicate renewed health")
		}
	}
	snap := stats.Snapshot()
	if snap.Slots[ConnPoolSize].ProbesSent != 2 || snap.Slots[ConnPoolSize].ProbeReplies != 2 || snap.Slots[ConnPoolSize].RepliesArrived != 0 || snap.Slots[2].RepliesArrived != 2 || snap.Slots[2].ProbesSent != 0 {
		t.Fatalf("origin/arrival confused: %+v", snap.Slots)
	}
	if snap.Slots[0].ReadPackets != 0 || snap.Slots[0].WritePackets != 0 {
		t.Fatal("inactive slot activity invented")
	}
	if snap.Slots[2].ReadBytes == 0 || snap.Slots[ConnPoolSize].WriteBytes == 0 {
		t.Fatal("missing byte counters")
	}

	// Unknown counter and previous-session nonce must not consume a real pending probe.
	reply := wire.echo(wire.send(false))
	before := stats.LastReply()
	unknown := bytes.Clone(reply)
	binary.BigEndian.PutUint64(unknown[len(unknown)-8:], 999999)
	wire.deliver(unknown)
	foreign := bytes.Clone(reply)
	foreign[echoOffset(foreign)+8+8] ^= 0xff
	wire.deliverBusiness(foreign)
	if !stats.LastReply().Equal(before) || stats.Snapshot().Pending != 1 {
		t.Fatal("unknown echo affected liveness or pending request")
	}

	// Expiry uses a deterministic local clock, not a timestamp carried by the peer.
	elapsed.Add(int64(probeLifetime))
	wire.deliver(reply)
	if !stats.LastReply().Equal(before) || stats.Snapshot().Pending != 0 || stats.Snapshot().Expired != 1 {
		t.Fatal("expired probe renewed health")
	}

	// Old reader AND old writer remain pinned after a generation transition.
	old := wire.echo(wire.send(true))
	if stats.BeginGeneration() != 1 || stats.Generation() != 1 || !stats.LastReply().IsZero() {
		t.Fatal("generation did not reset liveness")
	}
	wire.deliver(old)
	if !stats.LastReply().IsZero() {
		t.Fatal("old generation renewed health")
	}
	payload := (&clientTransport{dev: &tunDevice{addrsFn: func() (net.IP, net.IP, net.IP) { return net.ParseIP(regTestTunIPv4), nil, nil }}}).registrationPayloads(wire.ctx)[0]
	buf := config.LPool.Get().([]byte)
	copy(buf[datagramHeaderLen:], payload)
	buf[datagramHeaderLen] = packetTypeControl
	wire.in <- NewPacket(buf, len(payload), nil, nil)
	// FIFO business marker proves the retired writer processed the probe ahead of it.
	marker := config.LPool.Get().([]byte)
	marker[datagramHeaderLen] = 0
	wire.in <- NewPacket(marker, 1, nil, nil)
	select {
	case got := <-wire.requests:
		if len(got) != 1 {
			t.Fatal("old slot minted a new-generation probe")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writer barrier timed out")
	}
	if stats.Snapshot().Pending != 0 {
		t.Fatal("retired writer left pending probes")
	}
	fresh := newProbeWire(t, stats)
	fresh.deliver(old) // old payload on a current-generation arrival slot
	currentReply := fresh.echo(fresh.send(true))
	wire.deliver(currentReply) // current payload on a retired arrival slot
	if !stats.LastReply().IsZero() || stats.Snapshot().Pending != 1 {
		t.Fatal("retired arrival slot accepted a current probe")
	}
	wrongGeneration := bytes.Clone(currentReply)
	binary.BigEndian.PutUint64(wrongGeneration[len(wrongGeneration)-16:len(wrongGeneration)-8], 0)
	fresh.deliver(wrongGeneration)
	if !stats.LastReply().IsZero() || stats.Snapshot().Pending != 1 {
		t.Fatal("old generation payload consumed a current probe")
	}
	fresh.deliver(currentReply)
	if stats.LastReply().IsZero() {
		t.Fatal("new generation failed to recover")
	}
	// Returned snapshots cannot mutate live diagnostics.
	snap.Slots[2].RepliesArrived = 900
	if stats.Snapshot().Slots[2].RepliesArrived == 900 {
		t.Fatal("snapshot aliases live state")
	}
}

func TestHeartbeatIntegration_ClearPending(t *testing.T) {
	stats := &HeartbeatStats{}
	wire := newProbeWire(t, stats)
	wire.deliver(wire.echo(wire.send(false)))
	old := wire.echo(wire.send(true))
	before := stats.Snapshot()
	nonce, counter := stats.nonce, stats.counter
	stats.ClearPending()
	cleared := stats.Snapshot()
	if cleared.Pending != 0 || !cleared.LastReply.IsZero() || cleared.LastRTT != 0 {
		t.Fatalf("old evidence retained: %+v", cleared)
	}
	if cleared.Generation != before.Generation || cleared.Slots != before.Slots || cleared.Sent != before.Sent || cleared.Replies != before.Replies || stats.nonce != nonce || stats.counter != counter {
		t.Fatal("fresh evidence reset changed transport identity or diagnostics")
	}
	wire.deliver(old)
	if !stats.LastReply().IsZero() {
		t.Fatal("pre-change echo renewed health")
	}
	stats.RequestProbe()
	select {
	case <-stats.probeRequests():
	case <-time.After(time.Second):
		t.Fatal("fresh probe not requested")
	}
	wire.deliver(wire.echo(wire.send(false)))
	if stats.LastReply().IsZero() || stats.Generation() != before.Generation || stats.counter <= counter {
		t.Fatal("same transport did not accept fresh evidence")
	}
}

func TestHeartbeatIntegration_PendingBound(t *testing.T) {
	var elapsed atomic.Int64
	base := time.Now()
	stats := &HeartbeatStats{now: func() time.Time { return base.Add(time.Duration(elapsed.Load())) }}
	wire := newProbeWire(t, stats)
	var first, last []byte
	for i := 0; i < maxPendingProbes+2; i++ {
		request := wire.send(i%2 == 0)
		if i == 0 {
			first = wire.echo(request)
		}
		if i == maxPendingProbes+1 {
			last = wire.echo(request)
		}
	}
	s := stats.Snapshot()
	if s.Pending != maxPendingProbes || s.Evicted != 2 {
		t.Fatalf("unbounded pending probes: %+v", s)
	}
	wire.deliver(first)
	if !stats.LastReply().IsZero() {
		t.Fatal("evicted probe accepted")
	}
	elapsed.Add(int64(probeLifetime))
	wire.deliver(last)
	if !stats.LastReply().IsZero() || stats.Snapshot().Pending != 0 || stats.Snapshot().Expired != maxPendingProbes {
		t.Fatal("pending expiry failed")
	}
}

// delayWriteReturn still writes through a real framed pipe, but holds the return
// so an echo can arrive before the local Write reports success or failure.
type delayWriteReturn struct {
	net.Conn
	release <-chan error
}

func (c *delayWriteReturn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if err != nil {
		return n, err
	}
	return n, <-c.release
}

func TestHeartbeatIntegration_WriteOutcome(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "clear"} {
		t.Run(outcome, func(t *testing.T) {
			fail := outcome == "failure"
			cleared := outcome == "clear"
			stats := &HeartbeatStats{}
			wire := newProbeWire(t, stats)
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			gate := make(chan error, 1)
			framed, _ := NewUDPConnOverTCP(wire.ctx, &delayWriteReturn{Conn: client, release: gate})
			peer, _ := NewUDPConnOverTCP(wire.ctx, server)
			payload := (&clientTransport{dev: &tunDevice{addrsFn: func() (net.IP, net.IP, net.IP) { return net.ParseIP(regTestTunIPv4), nil, nil }}}).registrationPayloads(wire.ctx)[0]
			done := make(chan error, 1)
			go func() {
				_, err := (&connSlot{id: 1, stats: stats}).writePayload(framed, payload, stats.Generation(), true)
				done <- err
			}()
			peer.SetReadDeadline(time.Now().Add(2 * time.Second))
			buf := make([]byte, 256)
			n, err := peer.Read(buf)
			if err != nil {
				t.Fatal(err)
			}
			wire.deliver(wire.echo(buf[typePrefixLen:n]))
			if !stats.LastReply().IsZero() {
				t.Fatal("uncommitted Write renewed health")
			}
			if cleared {
				stats.ClearPending()
			}
			if fail {
				gate <- io.ErrClosedPipe
			} else {
				gate <- nil
			}
			if err := <-done; (err != nil) != fail {
				t.Fatalf("unexpected write error: %v", err)
			}
			s := stats.Snapshot()
			if s.Pending != 0 || s.LastReply.IsZero() != (fail || cleared) {
				t.Fatalf("incorrect write outcome: %+v", s)
			}
			if !fail && !cleared && (s.Slots[1].ProbeReplies != 1 || s.Slots[2].RepliesArrived != 1) {
				t.Fatal("registration origin not preserved")
			}
		})
	}
}

func TestHeartbeatIntegration_ControlWriteErrorAfterFullFrame(t *testing.T) {
	stats := &HeartbeatStats{}
	wire := newProbeWire(t, stats)
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	gate := make(chan error, 1)
	raw := &delayWriteReturn{Conn: client, release: gate}
	peer, _ := NewUDPConnOverTCP(wire.ctx, server)
	payload := (&clientTransport{dev: &tunDevice{addrsFn: func() (net.IP, net.IP, net.IP) { return net.ParseIP(regTestTunIPv4), nil, nil }}}).registrationPayloads(wire.ctx)[0]
	p := batchPacket(datagramHeaderLen+len(payload), 0)
	copy(p.data[datagramHeaderLen:], payload)
	p.data[datagramHeaderLen] = packetTypeControl
	q := make(chan *Packet, 1)
	q <- p
	close(q)
	errs := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&connSlot{id: ConnPoolSize, stats: stats, isControl: true, inbound: q}).writeToConn(wire.ctx, raw, errs)
	}()
	peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	n, err := peer.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	wire.deliver(wire.echo(buf[typePrefixLen:n]))
	if !stats.LastReply().IsZero() {
		t.Fatal("pending control write renewed health")
	}
	gate <- io.ErrClosedPipe
	<-done
	if err := <-errs; err == nil {
		t.Fatal("missing write error")
	}
	if s := stats.Snapshot(); s.Pending != 0 || !s.LastReply.IsZero() {
		t.Fatalf("failed full-frame write renewed health: %+v", s)
	}
	checkBatchRefs(t, []*Packet{p})
}

func TestHeartbeatIntegration_RegistrationReconnectStats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := newRegServer(ctx, t)
	t.Cleanup(srv.dropAllConns)
	stats := startRegClient(ctx, t, srv.port, net.ParseIP(regTestTunIPv4), net.ParseIP(regTestTunIPv6))
	waitFor(t, 10*time.Second, func() bool {
		s := stats.Snapshot()
		for i := 0; i < ConnPoolSize; i++ {
			if s.Slots[i].ProbesSent < 2 || !s.Slots[i].Connected {
				return false
			}
		}
		return !s.LastReply.IsZero()
	}, "all registration writes recorded")
	before := stats.Snapshot()
	srv.dropAllConns()
	waitFor(t, 10*time.Second, func() bool {
		s := stats.Snapshot()
		for i := 0; i <= ConnPoolSize; i++ {
			if !s.Slots[i].Connected || s.Slots[i].Reconnects == 0 || s.Slots[i].LastReconnect.IsZero() || s.Slots[i].LastReconnectReason == "" {
				return false
			}
		}
		return s.Replies > before.Replies
	}, "slot reconnect diagnostics and correlated recovery")
	if stats.Generation() != before.Generation {
		t.Fatal("ordinary slot reconnect changed session generation")
	}
	stats.RequestProbe()
	waitFor(t, 5*time.Second, func() bool { return stats.Snapshot().Slots[ConnPoolSize].ProbeReplies > 0 }, "control probe returned on data path")
}

func TestHeartbeatIntegration_CancelClearsSlotActivity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := newRegServer(ctx, t)
	t.Cleanup(srv.dropAllConns)
	stats := startRegClient(ctx, t, srv.port, net.ParseIP(regTestTunIPv4), nil)
	waitFor(t, 5*time.Second, func() bool {
		for _, slot := range stats.Snapshot().Slots {
			if !slot.Connected {
				return false
			}
		}
		return true
	}, "slots connected")
	cancel()
	waitFor(t, time.Second, func() bool {
		for _, slot := range stats.Snapshot().Slots {
			if slot.Connected {
				return false
			}
		}
		return true
	}, "canceled slots not reported connected")
}

func TestHeartbeatIntegration_SameSlotSurvivesGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := newRegServer(ctx, t)
	stats := &HeartbeatStats{}
	dev := &tunDevice{addrsFn: func() (net.IP, net.IP, net.IP) { return net.ParseIP(regTestTunIPv4), nil, nil }}
	slot := &connSlot{
		id: 0, stats: stats, inbound: make(chan *Packet, 4), tunOutbound: make(chan *Packet, 4),
		registrations: (&clientTransport{dev: dev}).registrationPayloads,
		forward:       &Forwarder{Addr: fmt.Sprintf("127.0.0.1:%d", srv.port), Connector: NewUDPOverTCPConnector(), Transporter: TCPTransporter(nil), MaxRetries: 1},
	}
	done := make(chan struct{})
	go func() { defer close(done); slot.run(ctx) }()
	t.Cleanup(func() {
		cancel()
		srv.dropAllConns()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("slot did not stop")
		}
	})
	waitFor(t, 5*time.Second, func() bool { return !stats.LastReply().IsZero() }, "initial registration echo")
	before := stats.Snapshot()
	generation := stats.BeginGeneration()
	srv.dropAllConns()
	waitFor(t, 2*time.Second, func() bool {
		s := stats.Snapshot()
		return s.Generation == generation && s.Slots[0].Generation == generation && s.Slots[0].Connected && s.Replies > before.Replies && !s.LastReply.IsZero()
	}, "same slot reconnected and correlated a next-generation probe")
	select {
	case <-done:
		t.Fatal("slot run exited on generation change")
	default:
	}
}

func TestHeartbeatIntegration_GenerationResetsSlotDiagnostics(t *testing.T) {
	stats := &HeartbeatStats{}
	wire := newProbeWire(t, stats)
	wire.deliver(wire.echo(wire.send(false)))
	for i := 0; i <= ConnPoolSize; i++ {
		stats.slotTransition(i, 0, false, "previous failure")
		stats.slotTransition(i, 0, true, "")
		stats.slotActivity(i, 0, true, 20)
		stats.slotActivity(i, 0, false, 30)
	}
	before := stats.Snapshot()
	generation := stats.BeginGeneration()
	reset := stats.Snapshot()
	for i, slot := range reset.Slots {
		if slot.Generation != generation || slot.Connected || !slot.LastRead.IsZero() || !slot.LastWrite.IsZero() || !slot.LastReconnect.IsZero() || slot.LastReconnectReason != "" || slot.LastRTT != 0 {
			t.Errorf("slot %d retained previous-generation diagnostics: %+v", i, slot)
		}
		old := before.Slots[i]
		if slot.ReadPackets != old.ReadPackets || slot.WritePackets != old.WritePackets || slot.ReadBytes != old.ReadBytes || slot.WriteBytes != old.WriteBytes || slot.Reconnects != old.Reconnects || slot.ProbesSent != old.ProbesSent || slot.ProbeReplies != old.ProbeReplies || slot.RepliesArrived != old.RepliesArrived {
			t.Errorf("slot %d lost cumulative counters", i)
		}
	}
	stats.slotTransition(2, generation, true, "")
	current := stats.Snapshot()
	stats.slotTransition(2, generation-1, false, "late old disconnect")
	stats.slotActivity(2, generation-1, true, 999)
	if got := stats.Snapshot(); got != current {
		t.Fatal("late old connection changed current diagnostics")
	}
}

func (w *probeWire) deliverBusiness(reply []byte) {
	w.t.Helper()
	w.server.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := w.server.Write(append([]byte{0}, reply...)); err != nil {
		w.t.Fatal(err)
	}
	select {
	case pkt := <-w.out:
		defer pkt.release()
		if !bytes.Equal(pkt.data[tunReserve:datagramHeaderLen+pkt.length], reply) {
			w.t.Fatal("business echo changed")
		}
	case <-time.After(time.Second):
		w.t.Fatal("foreign-nonce business echo swallowed")
	}
}

func TestHeartbeatIntegration_ForeignNonceBusinessEcho(t *testing.T) {
	stats := &HeartbeatStats{}
	wire := newProbeWire(t, stats)
	for _, v6 := range []bool{false, true} {
		request := wire.send(v6)
		off := echoOffset(request)
		payload := bytes.Clone(request[off+8:])
		payload[8] ^= 0xff
		var req *stack.PacketBuffer
		if v6 {
			req = buildICMPv6EchoRequest(tcpip.AddrFromSlice(net.ParseIP(regTestTunIPv6).To16()), tcpip.AddrFromSlice(config.RouterIP6.To16()), 7, 9, payload)
		} else {
			req = buildICMPv4EchoRequest(tcpip.AddrFromSlice(net.ParseIP(regTestTunIPv4).To4()), tcpip.AddrFromSlice(config.RouterIP.To4()), 7, 9, payload)
		}
		ip := bytes.Clone(req.Data().AsRange().ToSlice())
		req.DecRef()
		wire.deliverBusiness(wire.echo(ip))
		if !stats.LastReply().IsZero() {
			t.Fatal("foreign business echo renewed health")
		}
	}
}

func TestHeartbeatIntegration_BusinessActivityIndependentOfProbeLock(t *testing.T) {
	stats := &HeartbeatStats{}
	wire := newProbeWire(t, stats)
	// Simulate snapshot/probe bookkeeping holding the global lock. Ordinary
	// framed data must still reach the TUN using only its arrival-slot lock.
	stats.mu.Lock()
	defer stats.mu.Unlock()
	wire.deliverBusiness(buildIPv4Packet(net.IPv4(10, 0, 0, 1), net.ParseIP(regTestTunIPv4), []byte("business data")))
}

func TestHeartbeatIntegration_RequestProbeCancellation(t *testing.T) {
	stats := &HeartbeatStats{}
	for i := 0; i < 100; i++ {
		stats.RequestProbe()
	}
	ctx, cancel := context.WithCancel(context.Background())
	ct := &clientTransport{stats: stats, dev: &tunDevice{addrsFn: func() (net.IP, net.IP, net.IP) { return net.ParseIP(regTestTunIPv4), nil, nil }}, controlSlot: &connSlot{inbound: make(chan *Packet, 16)}}
	done := make(chan struct{})
	go func() { defer close(done); ct.heartbeats(ctx) }()
	read := func() {
		t.Helper()
		select {
		case p := <-ct.controlSlot.inbound:
			p.release()
		case <-time.After(time.Second):
			t.Fatal("heartbeat wake lost")
		}
	}
	read() // initial
	read() // coalesced pre-start request
	stats.RequestProbe()
	read()
	if stats.Snapshot().Pending != 0 {
		t.Fatal("queued but unsent probes became pending")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("heartbeat cancellation leaked")
	}
	stats.RequestProbe()
}
