package resources

type Listener struct {
	Name       string
	Address    string
	Port       uint32
	RouteNames []string
}

type Route struct {
	Name    string
	Value   string
	Cluster string
}

type Cluster struct {
	Name      string
	Endpoints []Endpoint
}

type Endpoint struct {
	UpstreamHost string
	UpstreamPort uint32
}
