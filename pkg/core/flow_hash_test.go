package core

// Tests for five-tuple based pool dispatch (flowKey / parseFiveTupleInline / flowHash)
// and its end-to-end effect on the real connection pool.
//
// Why this exists: the pool used to partition by ipHash(dst), so every flow to one hot
// service IP collapsed onto a single slot. Dispatch is now by five-tuple, spreading those
// flows across the pool while keeping each individual flow pinned to one slot (a hard
// requirement — the server runs an independent gvisor stack per pool conn).
//
// The pure-function tests assert the hashing contract; TestFlowHash_RealPool_Spreads
// drives the REAL runConnPool dispatch + connSlot.writeToConn over tracked conns.

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

// --- test packet builders ---

// buildIPv4PacketWithPorts builds a minimal IPv4 packet (IHL=5, no options) carrying a
// 4-byte L4 stub with the given proto and src/dst ports.
func buildIPv4PacketWithPorts(src, dst net.IP, proto uint8, srcPort, dstPort uint16) []byte {
	pkt := make([]byte, 20+4)
	pkt[0] = 0x45 // version=4, IHL=5
	binary.BigEndian.PutUint16(pkt[2:4], uint16(len(pkt)))
	pkt[8] = 64 // TTL
	pkt[9] = proto
	copy(pkt[12:16], src.To4())
	copy(pkt[16:20], dst.To4())
	binary.BigEndian.PutUint16(pkt[20:22], srcPort)
	binary.BigEndian.PutUint16(pkt[22:24], dstPort)
	return pkt
}

// buildIPv4Fragment builds an IPv4 packet with fragmentation markers. mf sets the
// More-Fragments flag; fragOffset is the 13-bit fragment offset (in 8-byte units).
func buildIPv4Fragment(src, dst net.IP, mf bool, fragOffset uint16) []byte {
	pkt := buildIPv4PacketWithPorts(src, dst, 6, 1111, 80)
	var flags uint16 = fragOffset & 0x1fff
	if mf {
		flags |= 0x2000
	}
	binary.BigEndian.PutUint16(pkt[6:8], flags)
	return pkt
}

// --- pure-function tests ---

func TestParseFiveTuple_TCPUDP(t *testing.T) {
	src := net.IPv4(10, 0, 0, 1)
	dst := net.IPv4(10, 0, 0, 2)
	for _, proto := range []uint8{6, 17, 132} { // TCP, UDP, SCTP
		pkt := buildIPv4PacketWithPorts(src, dst, proto, 12345, 443)
		key := parseFiveTupleInline(pkt)
		if !key.hasPorts {
			t.Fatalf("proto %d: expected hasPorts=true", proto)
		}
		if key.proto != proto || key.srcPort != 12345 || key.dstPort != 443 {
			t.Fatalf("proto %d: got %+v", proto, key)
		}
	}
}

func TestParseFiveTuple_IHLWithOptions(t *testing.T) {
	// IHL=6 (24-byte header): 4 bytes of IP options before the L4 header.
	pkt := make([]byte, 24+4)
	pkt[0] = 0x46 // version=4, IHL=6
	pkt[9] = 6    // TCP
	copy(pkt[12:16], net.IPv4(1, 1, 1, 1).To4())
	copy(pkt[16:20], net.IPv4(2, 2, 2, 2).To4())
	binary.BigEndian.PutUint16(pkt[24:26], 5555) // srcPort at L4 offset 24
	binary.BigEndian.PutUint16(pkt[26:28], 6666)
	key := parseFiveTupleInline(pkt)
	if !key.hasPorts || key.srcPort != 5555 || key.dstPort != 6666 {
		t.Fatalf("IHL=6 options not honored: %+v", key)
	}
}

func TestParseFiveTuple_NoPorts(t *testing.T) {
	src, dst := net.IPv4(10, 0, 0, 1), net.IPv4(10, 0, 0, 2)
	// ICMP (proto 1) has no ports.
	if k := parseFiveTupleInline(buildIPv4PacketWithPorts(src, dst, 1, 0, 0)); k.hasPorts {
		t.Fatal("ICMP should not have ports")
	}
	// First fragment with MF set: ports must be ignored even though proto is TCP.
	if k := parseFiveTupleInline(buildIPv4Fragment(src, dst, true, 0)); k.hasPorts {
		t.Fatal("MF fragment should fall back")
	}
	// Non-first fragment (offset>0): no L4 header present.
	if k := parseFiveTupleInline(buildIPv4Fragment(src, dst, false, 185)); k.hasPorts {
		t.Fatal("offset fragment should fall back")
	}
	// Truncated (header says TCP but no L4 bytes).
	short := make([]byte, 20)
	short[0] = 0x45
	short[9] = 6
	if k := parseFiveTupleInline(short); k.hasPorts {
		t.Fatal("truncated L4 should fall back")
	}
}

func TestParseFiveTuple_IPv6(t *testing.T) {
	pkt := make([]byte, 40+4)
	pkt[0] = 0x60 // version=6
	pkt[6] = 17   // next header = UDP
	binary.BigEndian.PutUint16(pkt[40:42], 7777)
	binary.BigEndian.PutUint16(pkt[42:44], 53)
	key := parseFiveTupleInline(pkt)
	if !key.hasPorts || key.srcPort != 7777 || key.dstPort != 53 {
		t.Fatalf("IPv6 UDP parse failed: %+v", key)
	}
	// Extension header (e.g. 44 = Fragment) is not walked -> fall back.
	pkt[6] = 44
	if k := parseFiveTupleInline(pkt); k.hasPorts {
		t.Fatal("IPv6 extension header should fall back")
	}
}

func TestFlowHash_SameFiveTuple_SameSlot(t *testing.T) {
	const slots = 4
	dst := net.IPv4(10, 0, 0, 5).To4()
	key := parseFiveTupleInline(buildIPv4PacketWithPorts(net.IPv4(10, 0, 0, 1), dst, 6, 33333, 80))
	want := flowHash(key, dst, slots)
	for i := 0; i < 1000; i++ {
		if got := flowHash(key, dst, slots); got != want {
			t.Fatalf("flowHash not deterministic: iter %d got %d want %d", i, got, want)
		}
	}
}

func TestFlowHash_SameDst_DiffSrcPort_Spreads(t *testing.T) {
	const slots = 4
	dst := net.IPv4(10, 0, 0, 5).To4()
	hits := make([]int, slots)
	for sp := 20000; sp < 20256; sp++ {
		key := parseFiveTupleInline(buildIPv4PacketWithPorts(net.IPv4(10, 0, 0, 1), dst, 6, uint16(sp), 80))
		hits[flowHash(key, dst, slots)]++
	}
	used := 0
	for _, h := range hits {
		if h > 0 {
			used++
		}
	}
	if used < slots {
		t.Fatalf("five-tuple hash should use all %d slots, used %d (hits=%v)", slots, used, hits)
	}
	// Baseline: dst-IP hash pins every one of these flows to a single slot.
	if used == 1 {
		t.Fatal("no spread")
	}
	t.Logf("flowHash spread across %d/%d slots: %v", used, slots, hits)
}

func TestFlowHash_FallbackEqualsIPHash(t *testing.T) {
	const slots = 4
	src, dst := net.IPv4(10, 0, 0, 1), net.IPv4(10, 0, 0, 9).To4()
	// ICMP, MF fragment, and offset fragment must all route exactly like ipHash(dst).
	want := ipHash(dst, slots)
	cases := map[string][]byte{
		"icmp":     buildIPv4PacketWithPorts(src, dst, 1, 0, 0),
		"mf-frag":  buildIPv4Fragment(src, dst, true, 0),
		"off-frag": buildIPv4Fragment(src, dst, false, 185),
	}
	for name, pkt := range cases {
		key := parseFiveTupleInline(pkt)
		if got := flowHash(key, dst, slots); got != want {
			t.Fatalf("%s: flowHash=%d, want ipHash=%d (fragments/ICMP must fall back)", name, got, want)
		}
	}
}

// --- integration: drive the real runConnPool dispatch ---

// makePoolPacket builds a *Packet in canonical pool layout (IP at data[tunReserve:]) for
// the given raw IP packet, mirroring what routeOutbound enqueues onto tunInbound.
func makePoolPacket(ip []byte) *Packet {
	buf := config.LPool.Get().([]byte)[:]
	copy(buf[tunReserve:], ip)
	buf[datagramHeaderLen] = packetTypeToGvisor
	src, dst, _, _ := netutil.ParseIPFast(ip)
	return NewPacket(buf, typePrefixLen+len(ip), src, dst)
}

// TestFlowHash_RealPool_Spreads wires the real clientTransport.runConnPool over a mock
// forwarder that hands out tracked packetConn pairs (one per pool slot). It pushes many
// flows to ONE dst IP with distinct src ports and asserts: (1) they land on >1 pool conn
// (spread), and (2) each individual flow stays on exactly one conn (affinity).
func TestFlowHash_RealPool_Spreads(t *testing.T) {
	const poolSize = 4
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var serverConns []net.Conn
	srvReady := make(chan struct{}, poolSize)

	f := &Forwarder{
		Addr:       "mock",
		MaxRetries: 1,
		Transporter: &mockTransporter{dialFn: func(_ context.Context, _ string) (net.Conn, error) {
			cliEnd, srvEnd := newPacketConnPair()
			mu.Lock()
			serverConns = append(serverConns, srvEnd)
			mu.Unlock()
			srvReady <- struct{}{}
			return cliEnd, nil
		}},
		Connector: NewUDPOverTCPConnector(),
	}

	device := &tunDevice{
		tun:         func() net.Conn { a, _ := newPacketConnPair(); return a }(),
		tunInbound:  make(chan *Packet, MaxSize),
		tunOutbound: make(chan *Packet, MaxSize),
		errChan:     make(chan error, 1),
	}
	ct := newClientTransport(device, f, nil)
	ct.poolSize = poolSize
	device.transport = ct
	go ct.runConnPool(ctx)

	// Wait for all pool conns to be dialed.
	for i := 0; i < poolSize; i++ {
		select {
		case <-srvReady:
		case <-time.After(5 * time.Second):
			t.Fatalf("pool not established: only %d/%d conns dialed", i, poolSize)
		}
	}
	mu.Lock()
	conns := append([]net.Conn(nil), serverConns...)
	mu.Unlock()

	// Per-conn readers: record which src ports arrive on each conn.
	flowsPerConn := make([]map[uint16]int, len(conns))
	var rmu sync.Mutex
	var rwg sync.WaitGroup
	for i, c := range conns {
		flowsPerConn[i] = map[uint16]int{}
		rwg.Add(1)
		go func(idx int, c net.Conn) {
			defer rwg.Done()
			buf := make([]byte, config.LargeBufferSize)
			for {
				n, err := c.Read(buf)
				if err != nil {
					return
				}
				if n < datagramHeaderLen+typePrefixLen {
					continue
				}
				ip := buf[tunReserve:n] // [len][type][IP]
				key := parseFiveTupleInline(ip)
				if key.hasPorts {
					rmu.Lock()
					flowsPerConn[idx][key.srcPort]++
					rmu.Unlock()
				}
			}
		}(i, c)
	}

	// Push 64 flows to the SAME dst IP:port, each with a distinct src port; 3 packets each
	// so the affinity check sees repeats of one five-tuple.
	const flows, repeats = 64, 3
	dst := net.IPv4(10, 0, 0, 5)
	for r := 0; r < repeats; r++ {
		for sp := 0; sp < flows; sp++ {
			ip := buildIPv4PacketWithPorts(net.IPv4(10, 0, 0, 1), dst, 6, uint16(20000+sp), 80)
			device.tunInbound <- makePoolPacket(ip)
		}
	}

	// Wait until all flows*repeats packets have been observed.
	want := flows * repeats
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := 0
		rmu.Lock()
		for _, m := range flowsPerConn {
			for _, c := range m {
				got += c
			}
		}
		rmu.Unlock()
		if got >= want {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	rmu.Lock()
	defer rmu.Unlock()
	usedConns := 0
	total := 0
	seen := map[uint16]int{} // src port -> which conn index
	for idx, m := range flowsPerConn {
		if len(m) > 0 {
			usedConns++
		}
		for sp, cnt := range m {
			total += cnt
			if prev, ok := seen[sp]; ok && prev != idx {
				t.Fatalf("flow srcPort %d split across conns %d and %d (affinity broken)", sp, prev, idx)
			}
			seen[sp] = idx
			if cnt != repeats {
				t.Fatalf("flow srcPort %d delivered %d times, want %d", sp, cnt, repeats)
			}
		}
	}
	if total != want {
		t.Fatalf("received %d packets, want %d", total, want)
	}
	if usedConns < 2 {
		t.Fatalf("flows did not spread: used %d conns (want >=2)", usedConns)
	}
	t.Logf("64 flows to one dst IP spread across %d/%d pool conns", usedConns, poolSize)
}
