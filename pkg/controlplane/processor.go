package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"reflect"
	"strconv"
	"time"

	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	log "github.com/sirupsen/logrus"
	utilcache "k8s.io/apimachinery/pkg/util/cache"
	"sigs.k8s.io/yaml"
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
	configList, err := ParseYaml(file.FilePath)
	if err != nil {
		p.logger.Errorf("error parsing yaml file: %+v", err)
		return err
	}
	for _, config := range configList {
		if len(config.Uid) == 0 {
			continue
		}
		lastConfig, ok := p.expireCache.Get(config.Uid)
		if ok && reflect.DeepEqual(lastConfig.(*Virtual), config) {
			marshal, _ := json.Marshal(config)
			p.logger.Debugf("config are same, not needs to update, config: %s", string(marshal))
			continue
		}
		p.logger.Debugf("update config, version %d, config %v", p.version, config)

		listeners, clusters, routes, endpoints := config.To()
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
			p.logger.Errorf("snapshot inconsistency: %v, err: %v", snapshot, err)
			return err
		}

		if err = snapshot.Consistent(); err != nil {
			p.logger.Errorf("snapshot inconsistency: %v, err: %v", snapshot, err)
			return err
		}
		p.logger.Debugf("will serve snapshot %+v, nodeID: %s", snapshot, config.Uid)
		if err = p.cache.SetSnapshot(context.Background(), config.Uid, snapshot); err != nil {
			p.logger.Errorf("snapshot error %q for %v", err, snapshot)
			return err
		}

		p.expireCache.Set(config.Uid, config, time.Minute*5)
	}
	return nil
}

func ParseYaml(file string) ([]*Virtual, error) {
	var virtualList = make([]*Virtual, 0)

	yamlFile, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("Error reading YAML file: %s\n", err)
	}

	err = yaml.Unmarshal(yamlFile, &virtualList)
	if err != nil {
		return nil, err
	}

	return virtualList, nil
}
