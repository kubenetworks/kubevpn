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
    healthchecker.go              proxy.go (Mapper)          connect.go Get()
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
    healthchecker.go              proxy.go (Mapper)          connect.go Get()
    syncFromCache()               getPortMappingFromCache()  informer.GetStore()
    (local cache read)            (local cache read)         + API fallback
```

## Implementation

### Shared Informer Factory

```go
// pkg/handler/connect.go

type ConnectOptions struct {
    // ... existing fields ...
    cmInformer cache.SharedInformer  // shared ConfigMap informer
}

func (c *ConnectOptions) GetConfigMapInformer() cache.SharedInformer {
    if c.cmInformer == nil {
        c.cmInformer = informerv1.NewFilteredConfigMapInformer(
            c.clientset, c.ManagerNamespace, 0, cache.Indexers{},
            func(options *metav1.ListOptions) {
                options.FieldSelector = fields.OneTermEqualSelector(
                    "metadata.name", config.ConfigMapPodTrafficManager,
                ).String()
            },
        )
        go c.cmInformer.Run(c.ctx.Done())
    }
    return c.cmInformer
}
```

The informer is created lazily on first access. Its lifecycle is tied to `c.ctx` — when the connection is cleaned up, the watch is closed automatically.

### Consumers

#### 1. HealthPeriod (health status cache)

```go
// pkg/handler/healthchecker.go

func (c *ConnectOptions) HealthPeriod(ctx context.Context, _ time.Duration) {
    ticker := time.NewTicker(time.Second * 30)
    for {
        select {
        case <-ticker.C:
            c.syncFromCache()           // read from shared informer cache
            c.HealthCheckOnce(ctx, 10s) // direct GET for connectivity check
        }
    }
}

func (c *ConnectOptions) syncFromCache() {
    items := c.GetConfigMapInformer().GetStore().List()
    // update healthStatus.cm from cache
}
```

#### 2. Mapper (Fargate SSH tunnel manager)

```go
// pkg/handler/proxy.go

func NewMapper(..., cmInformer cache.SharedInformer) *Mapper {
    return &Mapper{cmInformer: cmInformer, ...}
}

func (m *Mapper) Run(_ string) {
    // Register event handler on shared informer
    m.cmInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
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

#### 3. ConnectOptions.Get() (read ConfigMap data)

```go
func (c *ConnectOptions) Get(ctx context.Context, key string) (string, error) {
    // Try shared informer cache first
    items := c.GetConfigMapInformer().GetStore().List()
    for _, item := range items {
        if cm, ok := item.(*v1.ConfigMap); ok {
            return cm.Data[key], nil
        }
    }
    // Fallback to direct API call
    cm, err := c.clientset.CoreV1().ConfigMaps(c.ManagerNamespace).Get(...)
    return cm.Data[key], err
}
```

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
| ConfigMap reads (steady state) | ~16/min (polling) | ~2/min (fallback ticker) |
| ConfigMap change latency | 2-10s (polling interval) | <1s (watch push) |

## Pod Informer (Not Shared)

The Mapper's pod informer watches pods by **label selector** (the Service selector), which is specific to each proxied workload. This cannot be shared because different Mapper instances may watch different namespaces and label selectors. Each Mapper still creates its own pod informer — only the ConfigMap informer is shared.
