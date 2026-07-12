# Shared ConfigMap Informer Design

## Problem

The `kubevpn-traffic-manager` ConfigMap is the central coordination point for the client daemon. Before this optimization, 3 independent informers watched it simultaneously, and several code paths made direct GET calls:

```
Before:
                                     API Server
                                         │
              ┌──────────────────────────┼──────────────────────────┐
              │ watch #1                 │ watch #2                 │ watch #3
              ▼                          ▼                          ▼
    health sync                   proxy.go (Mapper)          connect.go Get()
    NewFilteredConfigMap-         NewFilteredConfigMap-       ConfigMaps().Get()
    Informer()                    Informer()                 (direct API call)
```

This created 3 long-lived watch connections to the API server for the same resource, plus additional GET calls.

## Solution

A single shared informer on `ConnectOptions`, created once and reused by all consumers:

```
After:
                                     API Server
                                         │
                                    watch #1 (single)
                                         │
                              ConnectOptions.cmInformer
                             (GetConfigMapInformer())
                                         │
              ┌──────────────────────────┼──────────────────────────┐
              │                          │                          │
    status.go (status)            proxy.go (Mapper)          connect.go Get()
    GetTrafficManagerConfigMap()  getPortMappingFromCache()  informer.GetStore()
    (cache-only, miss→nil)        (local cache read)         (cache-only, miss→"")
```

Reads are **cache-only** — there is no live-API fallback. The cache is warmed once at
connection establishment via `EnsureSynced` (see below); afterwards every read is served
from memory and a cache miss returns empty. The former direct-GET fallback was removed
because, when the cluster/TUN is unreachable, a doomed live GET (with no deadline on the
daemon RPC path) made every `status`-touching command block on the OS TCP timeout (~4.6s).

## Implementation

### Shared Informer Factory

```go
// pkg/handler/connect.go
type ConnectOptions struct {
    // ... existing fields ...
    configMapStore *ConfigMapStore  // shared ConfigMap informer + health checks
}

func (c *ConnectOptions) GetConfigMapInformer() cache.SharedInformer {
    return c.getConfigMapStore().GetInformer()
}

// pkg/handler/configmap_store.go
type ConfigMapStore struct {
    clientset        kubernetes.Interface
    managerNamespace string
    informerOnce     sync.Once       // thread-safe single initialization
    informer         cache.SharedInformer
    informerStop     chan struct{}    // independent lifecycle control
}
```

The informer is created lazily on first access via `sync.Once` (thread-safe). Its lifecycle is managed by `ConfigMapStore.Stop()` which closes the `informerStop` channel, called from `ConnectOptions.Cleanup()`.

### Consumers

#### 1. Mapper (Fargate SSH tunnel manager)

```go
// pkg/handler/proxy_mapper.go

func NewMapper(..., cmInformer cache.SharedInformer) *Mapper {
    return &Mapper{cmInformer: cmInformer, ...}
}

func (m *Mapper) Run() {
    // Register event handler on shared informer (return value checked — log and return on error)
    _, err := m.cmInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
        UpdateFunc: func(_, _ interface{}) { triggerReconcile() },
    })

    // Pod informer is still per-Mapper (different namespace/labels)
    podInformer := informerv1.NewFilteredPodInformer(...)
    go podInformer.Run(m.ctx.Done())

    // React to events from both informers
    for {
        select {
        case <-reconcileCh:
            portMapping := m.getPortMappingFromCache() // shared informer cache
            m.reconcilePodsFromInformer(podInformer)   // pod informer cache
        }
    }
}
```

#### 2. ConnectOptions.Get() / GetTrafficManagerConfigMap() (read ConfigMap data)

Reads are cache-only. The cache is warmed once at connect via `EnsureSynced` — the data
plane calls it in `DataSession.DoConnect` (before `getCIDR`), the control plane in
`forwardConnectToSudo` (after `CreateOutboundPod`/`UpgradeDeploy`). A cache miss returns
empty (`""` / `nil`), never a blocking live-API GET.

```go
// EnsureSynced starts the informer and blocks until its initial List completes,
// bounded by config.ConfigMapSyncTimeout. Called once at connection establishment.
// Fail fast: connect aborts on a sync timeout — the store has no live-API fallback, so a
// cache that never warms would read empty forever, and by this point the ConfigMap already
// exists so a timeout means the API is unreachable (connect would fail regardless).
func (s *ConfigMapStore) EnsureSynced(ctx context.Context) error { /* WaitForCacheSync */ }

func (c *ConnectOptions) Get(_ context.Context, key string) (string, error) {
    for _, item := range c.GetConfigMapInformer().GetStore().List() {
        if cm, ok := item.(*v1.ConfigMap); ok {
            return cm.Data[key], nil
        }
    }
    return "", nil // cache miss — no live-API fallback
}

// GetTrafficManagerConfigMap returns the whole ConfigMap from the warm cache; a miss
// returns (nil, nil). Used by daemon/action/status.go's buildProxyAndSyncStatus, which
// renders an empty proxy/sync list on a miss instead of blocking.
func (c *ConnectOptions) GetTrafficManagerConfigMap(ctx context.Context) (*v1.ConfigMap, error) {
    return c.getConfigMapStore().GetConfigMap(ctx)
}
```

> Status reporting (`kubevpn status`) reads the proxy/sync list via `GetTrafficManagerConfigMap` (warm informer cache, cache-only). Status liveness comes from the TUN heartbeat, not ConfigMap/API reachability — the former `HealthPeriod`/`HealthStatus` health-status cache has been removed. See [08-heartbeat-health.md](08-heartbeat-health.md). The user daemon's cross-daemon status hop (`getSudoTunIPs` → sudo daemon `Status`) is additionally bounded by `config.SudoStatusTimeout` so a wedged sudo daemon cannot stall a local command.

## What Stays as Direct API Calls

Read-modify-write operations **must** use direct API calls (not the informer cache) because they need optimistic concurrency control via `resourceVersion`:

| Operation | File | Reason |
|-----------|------|--------|
| `addEnvoyConfig` | `inject/envoy.go` | GET → modify → Update with RetryOnConflict |
| `removeEnvoyConfig` | `inject/envoy.go` | GET → modify → Update with RetryOnConflict |
| DHCP rent/release | `dhcp/dhcp.go` | GET → modify bitmap → Update with RetryOnConflict |
| Reset envoy rules | `handler/reset.go` | GET → modify → Update |
| `Set()` key-value | `handler/connect.go` | Patch with RetryOnConflict |

These cannot read from the eventually-consistent informer cache because a stale `resourceVersion` would cause the Update to fail or silently overwrite concurrent changes.

## API Server Impact

| Metric | Before | After |
|--------|--------|-------|
| Watch connections | 3 | 1 |
| ConfigMap reads (steady state) | ~16/min (polling) | ~0 (cache-only reads; one initial List per connect) |
| ConfigMap change latency | 2-10s (polling interval) | <1s (watch push) |

## Pod Informer (Not Shared)

The Mapper's pod informer watches pods by **label selector** (the Service selector), which is specific to each proxied workload. This cannot be shared because different Mapper instances may watch different namespaces and label selectors. Each Mapper still creates its own pod informer — only the ConfigMap informer is shared.
