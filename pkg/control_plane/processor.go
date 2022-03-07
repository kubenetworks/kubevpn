package control_plane

import (
	"context"
	"fmt"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/sirupsen/logrus"
	"io/ioutil"
	"k8s.io/apimachinery/pkg/util/yaml"
	"math"
	"math/rand"
	"strconv"
)

type Processor struct {
	cache   cache.SnapshotCache
	logger  *logrus.Logger
	version int64
}

func NewProcessor(cache cache.SnapshotCache, log *logrus.Logger) *Processor {
	return &Processor{
		cache:   cache,
		logger:  log,
		version: rand.Int63n(1000),
	}
}

func (p *Processor) newVersion() string {
	if p.version == math.MaxInt64 {
		p.version = 0
	}
	p.version++
	return strconv.FormatInt(p.version, 10)
}

func (p *Processor) ProcessFile(file NotifyMessage) {
	configList, err := ParseYaml(file.FilePath)
	if err != nil {
		p.logger.Errorf("error parsing yaml file: %+v", err)
		return
	}
	for _, config := range configList {
		if len(config.Uid) == 0 {
			continue
		}
		listeners, clusters, routes, endpoints := config.To()
		resources := map[resource.Type][]types.Resource{
			resource.ListenerType: listeners, // listeners
			resource.RouteType:    routes,    // routes
			resource.ClusterType:  clusters,  // clusters
			resource.EndpointType: endpoints, // endpoints
			resource.RuntimeType:  {},        // runtimes
			resource.SecretType:   {},        // secrets
		}
		snapshot, err := cache.NewSnapshot(p.newVersion(), resources)

		if err != nil {
			p.logger.Errorf("snapshot inconsistency: %v, err: %v", snapshot, err)
			return
		}

		if err = snapshot.Consistent(); err != nil {
			p.logger.Errorf("snapshot inconsistency: %v, err: %v", snapshot, err)
			return
		}
		p.logger.Debugf("will serve snapshot %+v, nodeID: %s", snapshot, config.Uid)
		if err = p.cache.SetSnapshot(context.TODO(), config.Uid, snapshot); err != nil {
			p.logger.Errorf("snapshot error %q for %v", err, snapshot)
			p.logger.Fatal(err)
		}
	}
}

func ParseYaml(file string) ([]*Virtual, error) {
	var virtualList = make([]*Virtual, 0)

	yamlFile, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("Error reading YAML file: %s\n", err)
	}

	err = yaml.Unmarshal(yamlFile, &virtualList)
	if err != nil {
		return nil, err
	}

	return virtualList, nil
}
