package dns

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	miekgdns "github.com/miekg/dns"
	"k8s.io/apimachinery/pkg/util/cache"
)

// memWriter is an in-memory miekgdns.ResponseWriter that captures the written response.
type memWriter struct{ msg *miekgdns.Msg }

func (m *memWriter) LocalAddr() net.Addr        { return &net.UDPAddr{} }
func (m *memWriter) RemoteAddr() net.Addr       { return &net.UDPAddr{} }
func (m *memWriter) WriteMsg(r *miekgdns.Msg) error { m.msg = r; return nil }
func (m *memWriter) Write([]byte) (int, error)  { return 0, nil }
func (m *memWriter) Close() error               { return nil }
func (m *memWriter) TsigStatus() error          { return nil }
func (m *memWriter) TsigTimersOnly(bool)        {}
func (m *memWriter) Hijack()                    {}

// startMockUpstream starts a UDP DNS server on a random port, wrapping handler with an atomic
// query counter. Returns the server IP, port, and a pointer to the query count.
func startMockUpstream(t *testing.T, handler miekgdns.HandlerFunc) (host, port string, count *int64) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var c int64
	srv := &miekgdns.Server{PacketConn: pc, Handler: miekgdns.HandlerFunc(func(w miekgdns.ResponseWriter, r *miekgdns.Msg) {
		atomic.AddInt64(&c, 1)
		handler(w, r)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	h, p, _ := net.SplitHostPort(pc.LocalAddr().String())
	return h, p, &c
}

func newTestServer(host, port string, search ...string) *server {
	return &server{
		dnsCache: cache.NewLRUExpireCache(dnsCacheCapacity),
		forwardDNS: &miekgdns.ClientConfig{
			Servers: []string{host},
			Port:    port,
			Search:  search,
		},
	}
}

func doQuery(s *server, name string, qtype uint16) *miekgdns.Msg {
	m := new(miekgdns.Msg)
	m.SetQuestion(name, qtype)
	w := &memWriter{}
	s.ServeDNS(w, m)
	return w.msg
}

// TestServeDNS_AnswerCache: a repeated query is served from cache with zero extra upstream hits,
// the response carries the query's ID and a decremented (still positive) TTL.
func TestServeDNS_AnswerCache(t *testing.T) {
	const realName = "authors.default.svc.cluster.local."
	host, port, count := startMockUpstream(t, func(w miekgdns.ResponseWriter, r *miekgdns.Msg) {
		msg := new(miekgdns.Msg)
		msg.SetReply(r)
		if r.Question[0].Name == realName {
			rr, _ := miekgdns.NewRR(realName + " 60 IN A 10.96.0.10")
			msg.Answer = []miekgdns.RR{rr}
		} else {
			msg.Rcode = miekgdns.RcodeNameError
		}
		_ = w.WriteMsg(msg)
	})
	s := newTestServer(host, port, "default.svc.cluster.local")

	r1 := doQuery(s, "authors.", miekgdns.TypeA)
	if r1 == nil || len(r1.Answer) != 1 {
		t.Fatalf("first query: expected 1 answer, got %+v", r1)
	}
	if got := r1.Answer[0].Header().Name; got != "authors." {
		t.Errorf("answer name = %q, want rewritten %q", got, "authors.")
	}
	afterFirst := atomic.LoadInt64(count)

	r2 := doQuery(s, "authors.", miekgdns.TypeA)
	if got := atomic.LoadInt64(count); got != afterFirst {
		t.Errorf("second (cached) query hit upstream: %d -> %d", afterFirst, got)
	}
	if len(r2.Answer) != 1 {
		t.Fatalf("cached response missing answer: %+v", r2)
	}
	if ttl := r2.Answer[0].Header().Ttl; ttl == 0 || ttl > 60 {
		t.Errorf("cached TTL = %d, want in (0,60] after decrement", ttl)
	}
}

// TestServeDNS_NegativeCache: an NXDOMAIN is cached (per SOA minimum), so the repeat is not
// forwarded upstream.
func TestServeDNS_NegativeCache(t *testing.T) {
	host, port, count := startMockUpstream(t, func(w miekgdns.ResponseWriter, r *miekgdns.Msg) {
		msg := new(miekgdns.Msg)
		msg.SetReply(r)
		msg.Rcode = miekgdns.RcodeNameError
		soa, _ := miekgdns.NewRR("cluster.local. 30 IN SOA ns.dns.cluster.local. hostmaster.cluster.local. 1 3600 600 86400 5")
		msg.Ns = []miekgdns.RR{soa}
		_ = w.WriteMsg(msg)
	})
	s := newTestServer(host, port, "default.svc.cluster.local")

	_ = doQuery(s, "nope.", miekgdns.TypeA)
	afterFirst := atomic.LoadInt64(count)
	if afterFirst == 0 {
		t.Fatal("first query did not reach upstream")
	}
	_ = doQuery(s, "nope.", miekgdns.TypeA)
	if got := atomic.LoadInt64(count); got != afterFirst {
		t.Errorf("second (negatively cached) query hit upstream: %d -> %d", afterFirst, got)
	}
}

// TestServeDNS_Singleflight: many concurrent identical misses collapse into one upstream resolution.
func TestServeDNS_Singleflight(t *testing.T) {
	const realName = "svc.default.svc.cluster.local."
	host, port, count := startMockUpstream(t, func(w miekgdns.ResponseWriter, r *miekgdns.Msg) {
		time.Sleep(100 * time.Millisecond) // keep concurrent queries overlapping
		msg := new(miekgdns.Msg)
		msg.SetReply(r)
		if r.Question[0].Name == realName {
			rr, _ := miekgdns.NewRR(realName + " 60 IN A 10.0.0.1")
			msg.Answer = []miekgdns.RR{rr}
		} else {
			msg.Rcode = miekgdns.RcodeNameError
		}
		_ = w.WriteMsg(msg)
	})
	s := newTestServer(host, port, "default.svc.cluster.local")

	const M = 20
	var wg sync.WaitGroup
	for i := 0; i < M; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); doQuery(s, "svc.", miekgdns.TypeA) }()
	}
	wg.Wait()

	// One resolution fans out to 2 names (bare + 1 search suffix). Without singleflight it would be
	// M*2 = 40. Allow slack for a race where a second resolution starts before the first caches.
	if c := atomic.LoadInt64(count); c > 4 {
		t.Errorf("singleflight failed to dedup: %d upstream queries for %d concurrent (want ~2)", c, M)
	}
}

// TestServeDNS_ReturnsOnFirstSuccess: the handler returns as soon as one branch answers, without
// waiting for a slow (3s) sibling branch.
func TestServeDNS_ReturnsOnFirstSuccess(t *testing.T) {
	const realName = "fast.default.svc.cluster.local."
	host, port, _ := startMockUpstream(t, func(w miekgdns.ResponseWriter, r *miekgdns.Msg) {
		msg := new(miekgdns.Msg)
		msg.SetReply(r)
		if r.Question[0].Name == realName {
			rr, _ := miekgdns.NewRR(realName + " 60 IN A 10.0.0.2")
			msg.Answer = []miekgdns.RR{rr}
		} else {
			time.Sleep(3 * time.Second) // slow sibling branch
			msg.Rcode = miekgdns.RcodeNameError
		}
		_ = w.WriteMsg(msg)
	})
	s := newTestServer(host, port, "default.svc.cluster.local")

	start := time.Now()
	r := doQuery(s, "fast.", miekgdns.TypeA)
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Errorf("ServeDNS waited for the slow branch: %v (want prompt return on first success)", elapsed)
	}
	if r == nil || len(r.Answer) != 1 {
		t.Fatalf("expected 1 answer, got %+v", r)
	}
}
