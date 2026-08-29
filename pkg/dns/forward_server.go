package dns

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	miekgdns "github.com/miekg/dns"
	"golang.org/x/sync/singleflight"
	"k8s.io/apimachinery/pkg/util/cache"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

const (
	// upstreamQueryTimeout bounds a single forwarded DNS query to the upstream resolver.
	upstreamQueryTimeout = 5 * time.Second
	// dnsCacheTTL caps how long a positive answer stays cached (upper bound on the record TTL).
	dnsCacheTTL = 30 * time.Minute
	// negativeCacheTTL caps how long a negative (NXDOMAIN/NODATA) answer stays cached. Search-domain
	// expansion produces many misses, so caching them (briefly) avoids re-fanning-out every time.
	negativeCacheTTL = 30 * time.Second
	// dnsCacheCapacity is the maximum number of cached DNS entries.
	dnsCacheCapacity = 10000

	maxUint32 = ^uint32(0)
)

// cacheEntry is a ready-to-serve DNS response: names are already rewritten back to the original
// queried name, so serving only needs to set the current query's ID and decrement TTLs.
type cacheEntry struct {
	msg      *miekgdns.Msg
	cachedAt time.Time
	ttl      uint32 // seconds this entry is valid (min RR TTL for positive; SOA/default for negative)
	negative bool
}

// github.com/docker/docker@v23.0.1+incompatible/libnetwork/network_windows.go:53
type server struct {
	dnsCache   *cache.LRUExpireCache
	forwardDNS *miekgdns.ClientConfig
	// sf collapses concurrent identical misses (same name+type+class) into a single upstream
	// resolution — all waiters share the one answer instead of each fanning out.
	sf singleflight.Group
}

func ListenAndServe(network, address string, forwardDNS *miekgdns.ClientConfig) error {
	if forwardDNS.Port == "" {
		forwardDNS.Port = strconv.Itoa(config.PortDNS)
	}
	return miekgdns.ListenAndServe(address, network, &server{
		dnsCache:   cache.NewLRUExpireCache(dnsCacheCapacity),
		forwardDNS: forwardDNS,
	})
}

// ServeDNS answers from the cache when possible, otherwise resolves upstream (deduplicated by
// singleflight) and caches the answer. Positive and negative answers are both cached with a TTL,
// so repeat queries never touch the upstream resolver.
func (s *server) ServeDNS(w miekgdns.ResponseWriter, m *miekgdns.Msg) {
	defer w.Close()
	if len(m.Question) == 0 {
		m.Response = true
		_ = w.WriteMsg(m)
		return
	}

	q := m.Question[0]
	key := cacheKey(q)

	if e, ok := s.getCache(key); ok {
		plog.G(context.Background()).Debugf("DNS cache hit: %s (negative=%v)", q.Name, e.negative)
		_ = w.WriteMsg(buildFromCache(e, m))
		return
	}

	// Deduplicate concurrent identical misses; the shared resolution populates the cache.
	v, _, _ := s.sf.Do(key, func() (interface{}, error) {
		// Re-check: a concurrent Do may have just resolved and cached it.
		if e, ok := s.getCache(key); ok {
			return e, nil
		}
		e := s.resolve(q.Name, m)
		if e.ttl > 0 { // ttl==0 marks a transient failure (SERVFAIL) we must not cache
			s.putCache(key, e)
		}
		return e, nil
	})

	_ = w.WriteMsg(buildFromCache(v.(*cacheEntry), m))
}

// resolve fans out the search-domain expansions across upstream servers concurrently, returns on
// the first answer (cancelling the rest), and rewrites answer names back to the original name.
func (s *server) resolve(originName string, m *miekgdns.Msg) *cacheEntry {
	ctx, cancel := context.WithTimeout(context.Background(), upstreamQueryTimeout)
	defer cancel()

	searchList := fix(originName, s.forwardDNS.Search)
	resultCh := make(chan *miekgdns.Msg, 1)
	var won atomic.Bool
	var lastNeg atomic.Pointer[miekgdns.Msg] // last real "no records" response (for negative SOA/TTL)
	var wg sync.WaitGroup

	for _, name := range searchList {
		for _, dnsAddr := range s.forwardDNS.Servers {
			wg.Add(1)
			go func(name, dnsAddr string) {
				defer wg.Done()
				msg := m.Copy()
				for i := range msg.Question {
					msg.Question[i].Name = name
				}
				answer, err := miekgdns.ExchangeContext(ctx, msg, net.JoinHostPort(dnsAddr, s.forwardDNS.Port))
				if err != nil {
					return
				}
				if len(answer.Answer) == 0 {
					lastNeg.Store(answer)
					return
				}
				if !won.CompareAndSwap(false, true) {
					return
				}
				for i := range answer.Answer {
					answer.Answer[i].Header().Name = originName
				}
				for i := range answer.Question {
					answer.Question[i].Name = originName
				}
				select {
				case resultCh <- answer:
				default:
				}
				cancel() // first winner: stop the other in-flight branches
			}(name, dnsAddr)
		}
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case ans := <-resultCh:
		cancel()
		plog.G(ctx).Debugf("DNS resolved: %s (%d answers)", originName, len(ans.Answer))
		return positiveEntry(ans)
	case <-done:
		select {
		case ans := <-resultCh:
			return positiveEntry(ans)
		default:
		}
		if neg := lastNeg.Load(); neg != nil {
			return negativeEntry(neg)
		}
		plog.G(ctx).Errorf("failed to resolve domain name: %s", originName)
		return servfailEntry()
	}
}

func positiveEntry(ans *miekgdns.Msg) *cacheEntry {
	return &cacheEntry{msg: ans, cachedAt: time.Now(), ttl: minAnswerTTL(ans), negative: false}
}

// negativeEntry caches a real "no records" upstream response (NXDOMAIN / NODATA). The negative TTL
// follows the authority-section SOA minimum, capped at negativeCacheTTL.
func negativeEntry(upstream *miekgdns.Msg) *cacheEntry {
	neg := new(miekgdns.Msg)
	neg.Response = true
	neg.Rcode = upstream.Rcode
	ttl := uint32(negativeCacheTTL.Seconds())
	for _, rr := range upstream.Ns {
		if soa, ok := rr.(*miekgdns.SOA); ok {
			neg.Ns = append(neg.Ns, miekgdns.Copy(soa))
			if soa.Minttl < ttl {
				ttl = soa.Minttl
			}
		}
	}
	if ttl == 0 {
		ttl = 1
	}
	return &cacheEntry{msg: neg, cachedAt: time.Now(), ttl: ttl, negative: true}
}

// servfailEntry is returned when no upstream responded at all (transient failure). ttl==0 signals
// the caller not to cache it, so the next query retries.
func servfailEntry() *cacheEntry {
	msg := new(miekgdns.Msg)
	msg.Response = true
	msg.Rcode = miekgdns.RcodeServerFailure
	return &cacheEntry{msg: msg, cachedAt: time.Now(), ttl: 0}
}

// buildFromCache produces the wire response for query m from a cached entry: a copy with the
// current query's ID, the query's question, and TTLs decremented by the time spent in cache.
func buildFromCache(e *cacheEntry, m *miekgdns.Msg) *miekgdns.Msg {
	resp := e.msg.Copy()
	resp.Id = m.Id
	resp.Response = true
	resp.Question = m.Question
	elapsed := uint32(time.Since(e.cachedAt).Seconds())
	decrementTTL(resp.Answer, elapsed)
	decrementTTL(resp.Ns, elapsed)
	decrementTTL(resp.Extra, elapsed)
	return resp
}

func decrementTTL(rrs []miekgdns.RR, elapsed uint32) {
	for _, rr := range rrs {
		h := rr.Header()
		if h.Ttl > elapsed {
			h.Ttl -= elapsed
		} else {
			h.Ttl = 1
		}
	}
}

func minAnswerTTL(msg *miekgdns.Msg) uint32 {
	min := maxUint32
	for _, rr := range msg.Answer {
		if t := rr.Header().Ttl; t < min {
			min = t
		}
	}
	if min == maxUint32 || min == 0 {
		min = 1
	}
	if capTTL := uint32(dnsCacheTTL.Seconds()); min > capTTL {
		min = capTTL
	}
	return min
}

func (s *server) getCache(key string) (*cacheEntry, bool) {
	v, ok := s.dnsCache.Get(key)
	if !ok {
		return nil, false
	}
	return v.(*cacheEntry), true
}

func (s *server) putCache(key string, e *cacheEntry) {
	s.dnsCache.Add(key, e, time.Duration(e.ttl)*time.Second)
}

func cacheKey(q miekgdns.Question) string {
	return q.Name + "|" + strconv.Itoa(int(q.Qtype)) + "|" + strconv.Itoa(int(q.Qclass))
}

// productpage.default.svc.cluster.local.
// mongo-headless.mongodb.default.svc.cluster.local.
func fix(name string, suffix []string) []string {
	var dot = "."
	for strings.HasSuffix(name, dot) {
		name = strings.TrimSuffix(name, dot)
	}
	var result = []string{fmt.Sprintf("%s.", name)}
	for _, s := range suffix {
		result = append(result, fmt.Sprintf("%s.%s.", name, s))
	}
	return result
}
