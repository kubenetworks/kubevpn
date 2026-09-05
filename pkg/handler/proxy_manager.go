package handler

import (
	"sync"
)

// ProxyManager tracks the client-side port Mappers for a connection's proxied K8s Service
// workloads. Sidecar injection/unpatch itself runs server-side (see inject_client.go); the
// only client-side runtime is the Mapper (SSH reverse tunnels from the sidecar back to the
// developer's machine), which this manages so it can be stopped on leave/disconnect.
type ProxyManager struct {
	mu        sync.Mutex
	workloads ProxyList
}

// newProxyManager creates an empty ProxyManager.
func newProxyManager() *ProxyManager {
	return &ProxyManager{workloads: make(ProxyList, 0)}
}

// Add appends a proxy workload (with its Mapper) to the managed list.
func (pm *ProxyManager) Add(proxy *Proxy) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.workloads.Add(proxy)
}

// Remove stops and drops the Mapper for the given namespace/workload.
func (pm *ProxyManager) Remove(ns, workload string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.workloads.Remove(ns, workload)
}

// Resources returns a copy of the current proxy workload list.
func (pm *ProxyManager) Resources() ProxyList {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	result := make(ProxyList, len(pm.workloads))
	copy(result, pm.workloads)
	return result
}

// StopAll stops every tracked Mapper and clears the list (used on disconnect).
func (pm *ProxyManager) StopAll() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for _, p := range pm.workloads {
		p.portMapper.Stop()
	}
	pm.workloads = pm.workloads[:0]
}
