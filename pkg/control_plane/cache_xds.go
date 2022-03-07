package control_plane

import (
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
)

type XDSCache struct {
	Listeners map[string]Listener
	Routes    map[string]Route
	Clusters  map[string]Cluster
	Endpoints map[string]Endpoint
}

func (xds *XDSCache) ClusterContents() []types.Resource {
	var r []types.Resource

	for _, c := range xds.Clusters {
		r = append(r, MakeCluster(c.Name))
	}

	return r
}

func (xds *XDSCache) RouteContents() []types.Resource {

	var routesArray []Route
	for _, r := range xds.Routes {
		routesArray = append(routesArray, r)
	}

	return []types.Resource{MakeRoute(routesArray)}
}

func (xds *XDSCache) ListenerContents() []types.Resource {
	var r []types.Resource

	for _, l := range xds.Listeners {
		r = append(r, MakeHTTPListener(l.Name, l.RouteNames[0], l.Address, l.Port))
	}

	return r
}

func (xds *XDSCache) EndpointsContents() []types.Resource {
	var r []types.Resource

	for _, c := range xds.Clusters {
		r = append(r, MakeEndpoint(c.Name, c.Endpoints))
	}

	return r
}

func (xds *XDSCache) AddListener(name string, routeNames []string, address string, port uint32) {
	xds.Listeners[name] = Listener{
		Name:       name,
		Address:    address,
		Port:       port,
		RouteNames: routeNames,
	}
}

func (xds *XDSCache) AddRoute(name string, headers []HeaderMatch, clusterName string) {
	var h []Header
	for _, header := range headers {
		h = append(h, Header{
			Key:   header.Key,
			Value: header.Value,
		})
	}
	xds.Routes[name] = Route{
		Name:    name,
		Headers: h,
		Cluster: clusterName,
	}
}

func (xds *XDSCache) AddCluster(name string) {
	xds.Clusters[name] = Cluster{
		Name: name,
	}
}

func (xds *XDSCache) AddEndpoint(clusterName, upstreamHost string, upstreamPort uint32) {
	cluster := xds.Clusters[clusterName]

	cluster.Endpoints = append(cluster.Endpoints, Endpoint{
		UpstreamHost: upstreamHost,
		UpstreamPort: upstreamPort,
	})

	xds.Clusters[clusterName] = cluster
}
