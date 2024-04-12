package dns

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
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
	logInterval rate.Sometimes      // Rate-limit logging about hitting the fwdSem limit
}

func ListenAndServe(network, address string, forwardDNS *miekgdns.ClientConfig) error {
	if forwardDNS.Port == "" {
		forwardDNS.Port = strconv.Itoa(53)
	}
	return miekgdns.ListenAndServe(address, network, &server{
		dnsCache:    cache.NewLRUExpireCache(1000),
		forwardDNS:  forwardDNS,
		client:      &miekgdns.Client{Net: "udp", Timeout: time.Second * 30},
		fwdSem:      semaphore.NewWeighted(maxConcurrent),
		logInterval: rate.Sometimes{Interval: logInterval},
	})
}

// ServeDNS consider using a cache
// eg: nslookup -port=56571 code.byted.org 127.0.0.1
func (s *server) ServeDNS(w miekgdns.ResponseWriter, m *miekgdns.Msg) {
	defer w.Close()
	if len(m.Question) == 0 {
		m.Response = true
		_ = w.WriteMsg(m)
		return
	}

	ctx, cancelFunc := context.WithTimeout(context.Background(), time.Second*5)
	defer cancelFunc()
	// limits the number of outstanding concurrent queries
	err := s.fwdSem.Acquire(ctx, 1)
	if err != nil {
		s.logInterval.Do(func() {
			log.Errorf("dns-server more than %v concurrent queries", maxConcurrent)
		})
		m.SetRcode(m, miekgdns.RcodeRefused)
		return
	}
	defer s.fwdSem.Release(1)

	var q = m.Question[0]
	var originName = q.Name

	searchList := fix(originName, s.forwardDNS.Search)
	if v, ok := s.dnsCache.Get(originName); ok {
		searchList = []string{v.(string)}
		log.Infof("use cache name: %s --> %s", originName, v.(string))
	}

	for _, name := range searchList {
		for _, dnsAddr := range s.forwardDNS.Servers {
			var msg = m.Copy()
			for i := 0; i < len(msg.Question); i++ {
				msg.Question[i].Name = name
			}

			var answer *miekgdns.Msg
			answer, _, err = s.client.ExchangeContext(context.Background(), msg, net.JoinHostPort(dnsAddr, s.forwardDNS.Port))
			if err != nil {
				log.Errorf("can not found dns name: %s: %v", name, err)
				continue
			}
			if len(answer.Answer) == 0 {
				log.Infof("dns answer is empty for name: %s", name)
				continue
			}

			s.dnsCache.Add(originName, name, time.Minute*30)
			log.Infof("add cache: %s --> %s", originName, name)

			for i := 0; i < len(answer.Answer); i++ {
				answer.Answer[i].Header().Name = originName
			}
			for i := 0; i < len(answer.Question); i++ {
				answer.Question[i].Name = originName
			}

			err = w.WriteMsg(answer)
			if err != nil {
				log.Errorf("failed to write response for name: %s: %v", name, err.Error())
			}
			return
		}
	}

	m.Response = true
	_ = w.WriteMsg(m)
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
