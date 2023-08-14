package dns

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/rand"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	miekgdns "github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/semaphore"
	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/util/cache"
)

var (
	maxConcurrent int64 = 1024
	logInterval         = 2 * time.Second
)

// github.com/docker/docker@v23.0.1+incompatible/libnetwork/network_windows.go:53
type server struct {
	dnsCache   *cache.LRUExpireCache
	forwardDNS *miekgdns.ClientConfig
	client     *miekgdns.Client

	fwdSem      *semaphore.Weighted // Limit the number of concurrent external DNS requests in-flight
	logInverval rate.Sometimes      // Rate-limit logging about hitting the fwdSem limit
}

func NewDNSServer(network, address string, forwardDNS *miekgdns.ClientConfig) error {
	return miekgdns.ListenAndServe(address, network, &server{
		dnsCache:    cache.NewLRUExpireCache(1000),
		forwardDNS:  forwardDNS,
		client:      &miekgdns.Client{Net: "udp", SingleInflight: true, Timeout: time.Second * 30},
		fwdSem:      semaphore.NewWeighted(maxConcurrent),
		logInverval: rate.Sometimes{Interval: logInterval},
	})
}

// ServeDNS consider using a cache
// eg: nslookup -port=56571 code.byted.org 127.0.0.1
func (s *server) ServeDNS(w miekgdns.ResponseWriter, r *miekgdns.Msg) {
	defer w.Close()
	if len(r.Question) == 0 {
		r.Response = true
		_ = w.WriteMsg(r)
		return
	}

	ctx, cancelFunc := context.WithTimeout(context.Background(), time.Second*5)
	defer cancelFunc()
	// limits the number of outstanding concurrent queries
	err := s.fwdSem.Acquire(ctx, 1)
	if err != nil {
		s.logInverval.Do(func() {
			log.Errorf("dns-server more than %v concurrent queries", maxConcurrent)
		})
		r.SetRcode(r, miekgdns.RcodeRefused)
		return
	}
	defer s.fwdSem.Release(1)

	var wg = &sync.WaitGroup{}

	var done = &atomic.Value{}
	done.Store(false)
	var q = r.Question[0]
	var originName = q.Name

	searchList := fix(originName, s.forwardDNS.Search)
	if v, ok := s.dnsCache.Get(originName); ok {
		searchList = []string{v.(string)}
	}

	for _, name := range searchList {
		// only should have dot [5,6]
		// productpage.default.svc.cluster.local.
		// mongo-headless.mongodb.default.svc.cluster.local.
		count := strings.Count(name, ".")
		if count < 5 || count > 6 {
			continue
		}

		for _, dnsAddr := range s.forwardDNS.Servers {
			wg.Add(1)
			go func(name, dnsAddr string) {
				defer wg.Done()
				var msg miekgdns.Msg
				marshal, _ := json.Marshal(r)
				_ = json.Unmarshal(marshal, &msg)
				for i := 0; i < len(msg.Question); i++ {
					msg.Question[i].Name = name
				}
				msg.Ns = nil
				msg.Extra = nil
				msg.Id = uint16(rand.Intn(math.MaxUint16 + 1))
				answer, _, err := s.client.ExchangeContext(context.Background(), &msg, net.JoinHostPort(dnsAddr, s.forwardDNS.Port))

				if err == nil && len(answer.Answer) != 0 {
					s.dnsCache.Add(originName, name, time.Hour*24*365*100) // never expire

					for i := 0; i < len(answer.Answer); i++ {
						answer.Answer[i].Header().Name = originName
					}
					for i := 0; i < len(answer.Question); i++ {
						answer.Question[i].Name = originName
					}

					r.Answer = answer.Answer
					r.Response = answer.Response
					r.Authoritative = answer.Authoritative
					r.AuthenticatedData = answer.AuthenticatedData
					r.CheckingDisabled = answer.CheckingDisabled
					r.Rcode = answer.Rcode
					r.Truncated = answer.Truncated
					r.RecursionDesired = answer.RecursionDesired
					r.RecursionAvailable = answer.RecursionAvailable
					r.Opcode = answer.Opcode
					r.Zero = answer.Zero

					select {
					case <-ctx.Done():
						return
					default:
						done.Store(true)
						err = w.WriteMsg(r)
						cancelFunc()
						return
					}
				}
				if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
					log.Debugf(err.Error())
				}
			}(name, dnsAddr)
		}
	}
	go func() {
		wg.Wait()
		cancelFunc()
	}()

	select {
	case <-ctx.Done():
	}
	if !done.Load().(bool) {
		r.Response = true
		_ = w.WriteMsg(r)
	}
}

func fix(domain string, suffix []string) (result []string) {
	result = []string{domain}
	for _, s := range suffix {
		result = append(result, strings.TrimSuffix(domain, ".")+"."+s+".")
	}
	return
}
