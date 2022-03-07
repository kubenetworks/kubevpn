package control_plane

type EnvoyConfig struct {
	NodeID string `yaml:"nodeID"`
	Spec   Spec   `yaml:"spec"`
}

type Spec struct {
	Listeners []ListenerTemp `yaml:"listeners"`
	Clusters  []ClusterTemp  `yaml:"clusters"`
}

type ListenerTemp struct {
	Name    string      `yaml:"name"`
	Address string      `yaml:"address"`
	Port    uint32      `yaml:"port"`
	Routes  []RouteTemp `yaml:"routes"`
}

type RouteTemp struct {
	Name        string        `yaml:"name"`
	Headers     []HeaderMatch `yaml:"headers"`
	ClusterName string        `yaml:"clusters"`
}

type HeaderMatch struct {
	Key   string `yaml:"key"`
	Value string `yaml:"value"`
}

type ClusterTemp struct {
	Name      string         `yaml:"name"`
	Endpoints []EndpointTemp `yaml:"endpoints"`
}

type EndpointTemp struct {
	Address string `yaml:"address"`
	Port    uint32 `yaml:"port"`
}
