package dns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	miekgdns "github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/cache"
)

type server struct {
	dnsCache   *cache.LRUExpireCache
	forwardDNS *miekgdns.ClientConfig
	c          *miekgdns.Client
}

func NewDNSServer(network, address string, forwardDNS *miekgdns.ClientConfig) error {
	return miekgdns.ListenAndServe(address, network, &server{
		dnsCache:   cache.NewLRUExpireCache(1000),
		forwardDNS: forwardDNS,
		c:          &miekgdns.Client{Net: "udp", SingleInflight: false},
	})
}

// ServeDNS consider using a cache
func (s *server) ServeDNS(w miekgdns.ResponseWriter, r *miekgdns.Msg) {
	defer w.Close()
	if len(r.Question) == 0 {
		r.Response = true
		_ = w.WriteMsg(r)
		return
	}

	ctx, cancelFunc := context.WithTimeout(context.Background(), time.Second*5)
	defer cancelFunc()

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
					msg.Question[i].Qtype = miekgdns.TypeA // IPV4
				}
				msg.Ns = nil
				msg.Extra = nil

				//msg.Id = uint16(rand.Intn(math.MaxUint16 + 1))
				client := miekgdns.Client{Net: "udp", Timeout: time.Second * 30}
				//r, _, err = client.ExchangeContext(ctx, m, a)
				answer, _, err := client.ExchangeContext(context.Background(), &msg, fmt.Sprintf("%s:%s", dnsAddr, s.forwardDNS.Port))

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
