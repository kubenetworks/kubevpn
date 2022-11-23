package dns

import (
	"context"
	"strings"
	"time"

	miekgdns "github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/cache"
	"k8s.io/apimachinery/pkg/util/sets"
)

type server struct {
	// todo using cache to speed up dns resolve process
	dnsCache   *cache.LRUExpireCache
	forwardDNS *miekgdns.ClientConfig
}

func NewDNSServer(network, address string, forwardDNS *miekgdns.ClientConfig) error {
	return miekgdns.ListenAndServe(address, network, &server{
		dnsCache:   cache.NewLRUExpireCache(1000),
		forwardDNS: forwardDNS,
	})
}

// ServeDNS consider using a cache
func (s *server) ServeDNS(w miekgdns.ResponseWriter, r *miekgdns.Msg) {
	//defer w.Close()
	ctx, cancelFunc := context.WithTimeout(context.Background(), time.Second*3)
	defer cancelFunc()

	for _, dnsAddr := range s.forwardDNS.Servers {
		var msg = new(miekgdns.Msg)
		*msg = *r
		go func(r miekgdns.Msg, dnsAddr string) {
			var q = r.Question[0]
			var originName = q.Name
			q.Name = fix(originName, s.forwardDNS.Search[0])
			r.Question = []miekgdns.Question{q}
			answer, err := miekgdns.Exchange(&r, dnsAddr+":53")
			if err == nil && len(answer.Answer) != 0 {
				if len(answer.Answer) != 0 {
					answer.Answer[0].Header().Name = originName
				}
				if len(answer.Question) != 0 {
					answer.Question[0].Name = originName
				}
				if ctx.Err() == nil {
					defer cancelFunc()
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
		}(*msg, dnsAddr)
	}
	<-ctx.Done()
	if ctx.Err() != context.Canceled {
		r.Response = true
		_ = w.WriteMsg(r)
	}
}

func fix(domain, suffix string) string {
	namespace := strings.Split(suffix, ".")[0]
	if sets.NewString(strings.Split(domain, ".")...).Has(namespace) {
		domain = domain[0:strings.LastIndex(domain, namespace)]
	}
	return strings.TrimSuffix(domain, ".") + "." + suffix + "."
}
