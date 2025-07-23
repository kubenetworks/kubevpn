package controlplane

import (
	"context"
	"encoding/json"
	"math"
	"math/rand"
	"reflect"
	"strconv"
	"time"

	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	log "github.com/sirupsen/logrus"
	utilcache "k8s.io/apimachinery/pkg/util/cache"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type Processor struct {
	cache   cache.SnapshotCache
	logger  *log.Logger
	version int64

	expireCache *utilcache.Expiring
}

func NewProcessor(cache cache.SnapshotCache, log *log.Logger) *Processor {
	return &Processor{
		cache:       cache,
		logger:      log,
		version:     rand.Int63n(1000),
		expireCache: utilcache.NewExpiring(),
	}
}

func (p *Processor) newVersion() string {
	if p.version == math.MaxInt64 {
		p.version = 0
	}
	p.version++
	return strconv.FormatInt(p.version, 10)
}

func (p *Processor) ProcessFile(file NotifyMessage) error {
	configList, err := ParseYaml(file.Content)
	if err != nil {
		p.logger.Errorf("failed to parse config file: %v", err)
		return err
	}
	enableIPv6, _ := util.DetectSupportIPv6()
	for _, config := range configList {
		if len(config.Uid) == 0 {
			continue
		}

		var marshal []byte
		marshal, err = json.Marshal(config)
		if err != nil {
			p.logger.Errorf("failed to marshal config: %v", err)
			return err
		}
		uid := util.GenEnvoyUID(config.Namespace, config.Uid)
		lastConfig, ok := p.expireCache.Get(uid)
		if ok && reflect.DeepEqual(lastConfig.(*Virtual), config) {
			p.logger.Infof("no need to update, config: %s", string(marshal))
			continue
		}

		p.logger.Infof("update config, version: %d, config: %s", p.version, marshal)

		listeners, clusters, routes, endpoints := config.To(enableIPv6, p.logger)
		resources := map[resource.Type][]types.Resource{
			resource.ListenerType: listeners, // listeners
			resource.RouteType:    routes,    // routes
			resource.ClusterType:  clusters,  // clusters
			resource.EndpointType: endpoints, // endpoints
			resource.RuntimeType:  {},        // runtimes
			resource.SecretType:   {},        // secrets
		}

		var snapshot *cache.Snapshot
		snapshot, err = cache.NewSnapshot(p.newVersion(), resources)
		if err != nil {
			p.logger.Errorf("failed to snapshot inconsistency: %v", err)
			return err
		}
		if err = snapshot.Consistent(); err != nil {
			p.logger.Errorf("failed to snapshot inconsistency: %v", err)
			return err
		}
		p.logger.Infof("will serve snapshot %+v, nodeID: %s", snapshot, uid)
		err = p.cache.SetSnapshot(context.Background(), uid, snapshot)
		if err != nil {
			p.logger.Errorf("snapshot error %q for %v", err, snapshot)
			return err
		}

		p.expireCache.Set(uid, config, time.Minute*5)
	}
	return nil
}

func ParseYaml(content string) ([]*Virtual, error) {
	var virtualList = make([]*Virtual, 0)

	err := yaml.Unmarshal([]byte(content), &virtualList)
	if err != nil {
		return nil, err
	}

	return virtualList, nil
}
