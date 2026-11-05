package discovery

// ServiceRecord is the route/DNS view of a Kubernetes service.
type ServiceRecord struct {
	Name         string
	Namespace    string
	ClusterIPs   []string
	ExternalName string
}

// Frame is a namespace snapshot or delta, independent of the streaming protocol.
type Frame struct {
	Snapshot           bool
	Enabled            bool
	Version            int64
	AddedPodCIDRs      []string
	RemovedPodCIDRs    []string
	UpsertedServices   []*ServiceRecord
	RemovedServiceKeys []string
}

func (b *Broadcaster) Done() <-chan struct{} { return b.ctx.Done() }
func (b *Broadcaster) Err() error            { return b.ctx.Err() }

// Close cancels all namespace generations and joins initialization and informers.
func (b *Broadcaster) Close() {
	b.mu.Lock()
	b.cancel()
	hubs := make([]*nsRouteHub, 0, len(b.hubs))
	for _, hub := range b.hubs {
		hubs = append(hubs, hub)
	}
	b.mu.Unlock()
	for _, hub := range hubs {
		hub.stop()
	}
	b.workers.Wait()
}
