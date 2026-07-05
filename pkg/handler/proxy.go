package handler

// Proxy represents a single proxied workload with its SSH tunnel mapper.
type Proxy struct {
	headers   map[string]string
	portMap   []string
	workload  string
	namespace string

	portMapper *Mapper
}

// ProxyList is a collection of active proxy workloads for a connection.
type ProxyList []*Proxy

func (l *ProxyList) Remove(ns, workload string) {
	if l == nil {
		return
	}
	for i := 0; i < len(*l); i++ {
		p := (*l)[i]
		if p.workload == workload && p.namespace == ns {
			p.portMapper.Stop()
			*l = append((*l)[:i], (*l)[i+1:]...)
			i--
		}
	}
}

func (l *ProxyList) Add(proxy *Proxy) {
	*l = append(*l, proxy)
}

// Resources identifies a workload by namespace and name for leave/unpatch operations.
type Resources struct {
	Namespace string
	Workload  string
}

func (l ProxyList) ToResources() []Resources {
	var resources []Resources
	for _, proxy := range l {
		resources = append(resources, Resources{
			Namespace: proxy.namespace,
			Workload:  proxy.workload,
		})
	}
	return resources
}
