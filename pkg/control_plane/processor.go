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
	"os"
	"strconv"
)

type Processor struct {
	cache cache.SnapshotCache

	// snapshotVersion holds the current version of the snapshot.
	snapshotVersion int64

	log logrus.FieldLogger

	xdsCache XDSCache
}

func NewProcessor(cache cache.SnapshotCache, log logrus.FieldLogger) *Processor {
	return &Processor{
		cache:           cache,
		snapshotVersion: rand.Int63n(1000),
		log:             log,
		xdsCache: XDSCache{
			Listeners: make(map[string]Listener),
			Clusters:  make(map[string]Cluster),
			Routes:    make(map[string]Route),
			Endpoints: make(map[string]Endpoint),
		},
	}
}

// newSnapshotVersion increments the current snapshotVersion
// and returns as a string.
func (p *Processor) newSnapshotVersion() string {

	// Reset the snapshotVersion if it ever hits max size.
	if p.snapshotVersion == math.MaxInt64 {
		p.snapshotVersion = 0
	}

	// Increment the snapshot version & return as string.
	p.snapshotVersion++
	return strconv.FormatInt(p.snapshotVersion, 10)
}

// ProcessFile takes a file and generates an xDS snapshot
func (p *Processor) ProcessFile(file NotifyMessage) {
	// Parse file into object
	envoyConfigList, err := ParseYaml(file.FilePath)
	if err != nil {
		p.log.Errorf("error parsing yaml file: %+v", err)
		return
	}

	for _, envoyConfig := range envoyConfigList {
		// Parse Listeners
		for _, l := range envoyConfig.Spec.Listeners {
			var lRoutes []string
			for _, lr := range l.Routes {
				lRoutes = append(lRoutes, lr.Name)
			}

			p.xdsCache.AddListener(l.Name, lRoutes, l.Address, l.Port)

			for _, r := range l.Routes {
				p.xdsCache.AddRoute(r.Name, r.Headers, r.ClusterName)
			}
		}

		// Parse Clusters
		for _, c := range envoyConfig.Spec.Clusters {
			p.xdsCache.AddCluster(c.Name)

			// Parse endpoints
			for _, e := range c.Endpoints {
				p.xdsCache.AddEndpoint(c.Name, e.Address, e.Port)
			}
		}
		a := map[resource.Type][]types.Resource{
			resource.EndpointType: p.xdsCache.EndpointsContents(), // endpoints
			resource.ClusterType:  p.xdsCache.ClusterContents(),   // clusters
			resource.RouteType:    p.xdsCache.RouteContents(),     // routes
			resource.ListenerType: p.xdsCache.ListenerContents(),  // listeners
			resource.RuntimeType:  {},                             // runtimes
			resource.SecretType:   {},                             // secrets
		}
		// Create the snapshot that we'll serve to Envoy
		snapshot, err := cache.NewSnapshot(p.newSnapshotVersion(), a)

		if err != nil {
			p.log.Errorf("snapshot inconsistency err: %+v\n\n\n%+v", snapshot, err)
			return
		}

		if err = snapshot.Consistent(); err != nil {
			p.log.Errorf("snapshot inconsistency: %+v\n\n\n%+v", snapshot, err)
			return
		}
		p.log.Debugf("will serve snapshot %+v", snapshot)

		// Add the snapshot to the cache
		if err = p.cache.SetSnapshot(context.TODO(), envoyConfig.NodeID, snapshot); err != nil {
			p.log.Errorf("snapshot error %q for %+v", err, snapshot)
			os.Exit(1)
		}
	}
}

// ParseYaml takes in a yaml envoy config and returns a typed version
func ParseYaml(file string) ([]*EnvoyConfig, error) {
	var envoyConfigs = make([]*EnvoyConfig, 0)

	yamlFile, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("Error reading YAML file: %s\n", err)
	}

	err = yaml.Unmarshal(yamlFile, &envoyConfigs)
	if err != nil {
		return nil, err
	}

	return envoyConfigs, nil
}
