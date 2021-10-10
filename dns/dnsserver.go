package dns

import (
	"fmt"
	miekgdns "github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/cache"
	"strings"
)

type server struct {
	// todo using cache to speed up dns resolve process
	dnsCache   *cache.LRUExpireCache
	forwardDNS string
	namespace  string
}

func NewDNSServer(network, address, forwardDNS, namespace string) error {
	if len(namespace) == 0 {
		namespace = v1.NamespaceDefault
	}
	return miekgdns.ListenAndServe(address, network, &server{
		dnsCache: cache.NewLRUExpireCache(1000), forwardDNS: forwardDNS, namespace: namespace,
	})
}

// ServeDNS consider using a cache
func (s *server) ServeDNS(w miekgdns.ResponseWriter, r *miekgdns.Msg) {
	q := r.Question
	r.Question = make([]miekgdns.Question, 0, len(q))
	question := q[0]
	name := question.Name
	switch strings.Count(question.Name, ".") {
	case 1:
		question.Name = question.Name + s.namespace + ".svc.cluster.local."
	case 2:
		question.Name = question.Name + "svc.cluster.local."
	case 3:
		question.Name = question.Name + "cluster.local."
	case 4:
		question.Name = question.Name + "local."
	case 5:
	}
	r.Question = []miekgdns.Question{question}
	fmt.Println(r.Question)
	answer, err := miekgdns.Exchange(r, s.forwardDNS)
	if err != nil {
		log.Warnln(err)
		err = w.WriteMsg(r)
		if err != nil {
			log.Warnln(err)
		}
	} else {
		if len(answer.Answer) != 0 {
			answer.Answer[0].Header().Name = name
		}
		if len(answer.Question) != 0 {
			answer.Question[0].Name = name
		}
		err = w.WriteMsg(answer)
		if err != nil {
			log.Warnln(err)
		}
	}
}
