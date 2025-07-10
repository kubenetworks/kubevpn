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
	"k8s.io/apimachinery/pkg/util/cache"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// github.com/docker/docker@v23.0.1+incompatible/libnetwork/network_windows.go:53
type server struct {
	dnsCache   *cache.LRUExpireCache
	forwardDNS *miekgdns.ClientConfig
}

func ListenAndServe(network, address string, forwardDNS *miekgdns.ClientConfig) error {
	if forwardDNS.Port == "" {
		forwardDNS.Port = strconv.Itoa(53)
	}
	return miekgdns.ListenAndServe(address, network, &server{
		dnsCache:   cache.NewLRUExpireCache(1000 * 1000),
		forwardDNS: forwardDNS,
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

	var q = m.Question[0]
	var originName = q.Name

	ctx, cancelFunc := context.WithTimeout(context.Background(), time.Second*5)
	defer cancelFunc()

	var wg = &sync.WaitGroup{}
	var isSuccess = &atomic.Bool{}

	var searchList []string
	if v, ok := s.dnsCache.Get(originName); ok {
		searchList = []string{v.(string)}
		plog.G(ctx).Infof("Use cache name: %s --> %s", originName, v.(string))
	} else {
		searchList = fix(originName, s.forwardDNS.Search)
	}

	for _, name := range searchList {
		for _, dnsAddr := range s.forwardDNS.Servers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var msg = m.Copy()
				for i := 0; i < len(msg.Question); i++ {
					msg.Question[i].Name = name
				}

				answer, err := miekgdns.ExchangeContext(ctx, msg, net.JoinHostPort(dnsAddr, s.forwardDNS.Port))
				if err != nil {
					plog.G(ctx).Errorf("Failed to found DNS name: %s: %v", name, err)
					return
				}
				if len(answer.Answer) == 0 {
					return
				}
				if !isSuccess.CompareAndSwap(false, true) {
					return
				}

				s.dnsCache.Add(originName, name, time.Minute*30)

				for i := 0; i < len(answer.Answer); i++ {
					answer.Answer[i].Header().Name = originName
				}
				for i := 0; i < len(answer.Question); i++ {
					answer.Question[i].Name = originName
				}
				plog.G(ctx).Infof("Resolve domain %s with full name: %s --> %s", originName, name, answer.String())

				err = w.WriteMsg(answer)
				if err != nil {
					plog.G(context.Background()).Errorf("Failed to write response for name: %s: %v", name, err.Error())
				}
			}()
		}
	}

	wg.Wait()

	if !isSuccess.Load() {
		plog.G(ctx).Errorf("can't found domain name: %s", originName)
		m.Response = true
		_ = w.WriteMsg(m)
	}
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
