package xds

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

// snapshotCacheTTL is how long a generated xDS snapshot stays cached before expiry.
const snapshotCacheTTL = 5 * time.Minute

// Processor converts Virtual configs into envoy xDS snapshots.
type Processor struct {
	cache   cache.SnapshotCache
	logger  *log.Entry
	version int64

	expireCache *utilcache.Expiring
	knownUIDs   map[string]bool
}

func newProcessor(cache cache.SnapshotCache, log *log.Entry) *Processor {
	return &Processor{
		cache:       cache,
		logger:      log,
		version:     rand.Int63n(1000),
		expireCache: utilcache.NewExpiring(),
		knownUIDs:   make(map[string]bool),
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
	configList, err := parseYaml(file.Content)
	if err != nil {
		p.logger.Errorf("failed to parse config file: %v", err)
		return err
	}

	activeUIDs := make(map[string]bool)

	enableIPv6, _ := util.DetectSupportIPv6()
	for _, config := range configList {
		if len(config.UID) == 0 {
			continue
		}
		if config.SchemaVersion == 0 {
			p.logger.Warnf("legacy envoy config without schema version for %s/%s, consider re-proxying to upgrade", config.Namespace, config.UID)
		} else if config.SchemaVersion > CurrentSchemaVersion {
			p.logger.Warnf("envoy config for %s/%s has schema version %d, but this build only supports up to %d", config.Namespace, config.UID, config.SchemaVersion, CurrentSchemaVersion)
		}

		var marshal []byte
		marshal, err = json.Marshal(config)
		if err != nil {
			p.logger.Errorf("failed to marshal config: %v", err)
			return err
		}
		uid := util.GenEnvoyUID(config.Namespace, config.UID)
		activeUIDs[uid] = true
		lastConfig, ok := p.expireCache.Get(uid)
		if ok && reflect.DeepEqual(lastConfig.(*Virtual), config) {
			p.logger.Debugf("config unchanged for %s, skipping", uid)
			continue
		}

		p.logger.Infof("updating xDS config for %s, version: %d", uid, p.version)
		p.logger.Debugf("config detail: %s", marshal)

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
		p.logger.Infof("serving xDS snapshot version %d for %s", p.version, uid)
		err = p.cache.SetSnapshot(context.Background(), uid, snapshot)
		if err != nil {
			p.logger.Errorf("snapshot error %q for %v", err, snapshot)
			return err
		}

		p.expireCache.Set(uid, config, snapshotCacheTTL)
	}

	for uid := range p.knownUIDs {
		if !activeUIDs[uid] {
			p.cache.ClearSnapshot(uid)
			// Also drop the expire-cache entry. Otherwise a later re-add of an
			// identical Virtual (e.g. a quick leave→re-proxy/sync of the same
			// workload) would match the DeepEqual check above, be treated as
			// "unchanged", and skip SetSnapshot — leaving the snapshot cleared
			// until the TTL expires, so envoy keeps serving without routes.
			p.expireCache.Delete(uid)
			delete(p.knownUIDs, uid)
			p.logger.Infof("cleared stale xDS snapshot for %s", uid)
		}
	}
	for uid := range activeUIDs {
		p.knownUIDs[uid] = true
	}

	return nil
}

func parseYaml(content string) ([]*Virtual, error) {
	virtualList := make([]*Virtual, 0)

	err := yaml.Unmarshal([]byte(content), &virtualList)
	if err != nil {
		return nil, err
	}

	return virtualList, nil
}
