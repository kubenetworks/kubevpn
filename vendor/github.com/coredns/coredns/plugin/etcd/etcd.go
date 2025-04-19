// Package etcd provides the etcd version 3 backend plugin.
package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/etcd/msg"
	"github.com/coredns/coredns/plugin/pkg/fall"
	"github.com/coredns/coredns/plugin/pkg/upstream"
	"github.com/coredns/coredns/request"

	"github.com/miekg/dns"
	"go.etcd.io/etcd/api/v3/mvccpb"
	etcdcv3 "go.etcd.io/etcd/client/v3"
)

const (
	priority    = 10  // default priority when nothing is set
	ttl         = 300 // default ttl when nothing is set
	etcdTimeout = 5 * time.Second
)

var errKeyNotFound = errors.New("key not found")

// Etcd is a plugin talks to an etcd cluster.
type Etcd struct {
	Next       plugin.Handler
	Fall       fall.F
	Zones      []string
	PathPrefix string
	Upstream   *upstream.Upstream
	Client     *etcdcv3.Client

	endpoints []string // Stored here as well, to aid in testing.
}

// Services implements the ServiceBackend interface.
func (e *Etcd) Services(ctx context.Context, state request.Request, exact bool, opt plugin.Options) (services []msg.Service, err error) {
	services, err = e.Records(ctx, state, exact)
	if err != nil {
		return
	}

	services = msg.Group(services)
	return
}

// Reverse implements the ServiceBackend interface.
func (e *Etcd) Reverse(ctx context.Context, state request.Request, exact bool, opt plugin.Options) (services []msg.Service, err error) {
	return e.Services(ctx, state, exact, opt)
}

// Lookup implements the ServiceBackend interface.
func (e *Etcd) Lookup(ctx context.Context, state request.Request, name string, typ uint16) (*dns.Msg, error) {
	return e.Upstream.Lookup(ctx, state, name, typ)
}

// IsNameError implements the ServiceBackend interface.
func (e *Etcd) IsNameError(err error) bool {
	return err == errKeyNotFound
}

// Records looks up records in etcd. If exact is true, it will lookup just this
// name. This is used when find matches when completing SRV lookups for instance.
func (e *Etcd) Records(ctx context.Context, state request.Request, exact bool) ([]msg.Service, error) {
	name := state.Name()

	path, star := msg.PathWithWildcard(name, e.PathPrefix)
	r, err := e.get(ctx, path, !exact)
	if err != nil {
		return nil, err
	}
	segments := strings.Split(msg.Path(name, e.PathPrefix), "/")
	return e.loopNodes(r.Kvs, segments, star, state.QType())
}

func (e *Etcd) get(ctx context.Context, path string, recursive bool) (*etcdcv3.GetResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, etcdTimeout)
	defer cancel()
	if recursive {
		if !strings.HasSuffix(path, "/") {
			path = path + "/"
		}
		r, err := e.Client.Get(ctx, path, etcdcv3.WithPrefix())
		if err != nil {
			return nil, err
		}
		if r.Count == 0 {
			path = strings.TrimSuffix(path, "/")
			r, err = e.Client.Get(ctx, path)
			if err != nil {
				return nil, err
			}
			if r.Count == 0 {
				return nil, errKeyNotFound
			}
		}
		return r, nil
	}

	r, err := e.Client.Get(ctx, path)
	if err != nil {
		return nil, err
	}
	if r.Count == 0 {
		return nil, errKeyNotFound
	}
	return r, nil
}

func (e *Etcd) loopNodes(kv []*mvccpb.KeyValue, nameParts []string, star bool, qType uint16) (sx []msg.Service, err error) {
	bx := make(map[msg.Service]struct{})
Nodes:
	for _, n := range kv {
		if star {
			s := string(n.Key)
			keyParts := strings.Split(s, "/")
			for i, n := range nameParts {
				if i > len(keyParts)-1 {
					// name is longer than key
					continue Nodes
				}
				if n == "*" || n == "any" {
					continue
				}
				if keyParts[i] != n {
					continue Nodes
				}
			}
		}
		serv := new(msg.Service)
		if err := json.Unmarshal(n.Value, serv); err != nil {
			return nil, fmt.Errorf("%s: %s", n.Key, err.Error())
		}
		serv.Key = string(n.Key)
		if _, ok := bx[*serv]; ok {
			continue
		}
		bx[*serv] = struct{}{}

		serv.TTL = e.TTL(n, serv)
		if serv.Priority == 0 {
			serv.Priority = priority
		}

		if shouldInclude(serv, qType) {
			sx = append(sx, *serv)
		}
	}
	return sx, nil
}

// TTL returns the smaller of the etcd TTL and the service's
// TTL. If neither of these are set (have a zero value), a default is used.
func (e *Etcd) TTL(kv *mvccpb.KeyValue, serv *msg.Service) uint32 {
	etcdTTL := uint32(kv.Lease)

	if etcdTTL == 0 && serv.TTL == 0 {
		return ttl
	}
	if etcdTTL == 0 {
		return serv.TTL
	}
	if serv.TTL == 0 {
		return etcdTTL
	}
	if etcdTTL < serv.TTL {
		return etcdTTL
	}
	return serv.TTL
}

// shouldInclude returns true if the service should be included in a list of records, given the qType. For all the
// currently supported lookup types, the only one to allow for an empty Host field in the service are TXT records
// which resolve directly.  If a TXT record is being resolved by CNAME, then we expect the Host field to have a
// value while the TXT field will be empty.
func shouldInclude(serv *msg.Service, qType uint16) bool {
	return (qType == dns.TypeTXT && serv.Text != "") || serv.Host != ""
}

// OnShutdown shuts down etcd client when caddy instance restart
func (e *Etcd) OnShutdown() error {
	if e.Client != nil {
		e.Client.Close()
	}
	return nil
}
