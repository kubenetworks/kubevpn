package dns

import (
	"context"
	"fmt"
	"strings"
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
		c:          &miekgdns.Client{Net: "udp" /*, SingleInflight: true*/},
	})
}

// ServeDNS consider using a cache
func (s *server) ServeDNS(w miekgdns.ResponseWriter, r *miekgdns.Msg) {
	if len(r.Question) == 0 {
		r.Response = true
		_ = w.WriteMsg(r)
		return
	}

	get, b := s.dnsCache.Get(r.Question[0].Name)
	if b {
		r.Response = true
		r.Answer = get.([]miekgdns.RR)
		_ = w.WriteMsg(r)
		return
	}

	ctx, cancelFunc := context.WithTimeout(context.Background(), time.Second*5)
	defer cancelFunc()

	var ok = &atomic.Value{}
	ok.Store(false)
	var q = r.Question[0]
	var originName = q.Name
	for _, dnsAddr := range s.forwardDNS.Servers {
		for _, name := range fix(originName, s.forwardDNS.Search) {
			go func(name, dnsAddr string) {
				var msg = new(miekgdns.Msg)
				*msg = *r
				q.Name = name
				r.Question = []miekgdns.Question{q}
				answer, _, err := s.c.ExchangeContext(ctx, msg, fmt.Sprintf("%s:%s", dnsAddr, s.forwardDNS.Port))
				if err == nil && len(answer.Answer) != 0 {
					for i := 0; i < len(answer.Answer); i++ {
						answer.Answer[i].Header().Name = originName
					}
					//answer.Answer[0].Header().Name = originName
					if len(answer.Question) != 0 {
						answer.Question[0].Name = originName
					}
					if ctx.Err() == nil {
						defer cancelFunc()
						ok.Store(true)
						err = w.WriteMsg(answer)
						if err != nil {
							log.Debugf(err.Error())
						}
					}
					return
				}
				if err != nil {
					log.Debugf(err.Error())
				}
			}(name, dnsAddr)
		}
	}
	<-ctx.Done()
	if !ok.Load().(bool) {
		r.Response = true
		_ = w.WriteMsg(r)
	} else {
		s.dnsCache.Add(r.Question[0].Name, r.Answer, time.Second*1)
	}
}

func fix(domain string, suffix []string) (result []string) {
	result = []string{domain}
	for _, s := range suffix {
		result = append(result, strings.TrimSuffix(domain, ".")+"."+s+".")
	}
	return
}
