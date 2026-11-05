package action

import (
	"maps"
	"slices"

	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

// connectionRecord is the historical on-disk JSON contract, independent of the
// runtime session. Replay uses RequestRaw and OwnerID; the remaining configuration
// is retained for compatibility with existing files and consumers.
type connectionRecord struct {
	RequestRaw           []byte `json:"RequestRaw,omitempty"`
	ManagerNamespace     string
	WorkloadNamespace    string
	ExtraRouteInfo       routeRecord
	OriginKubeconfigPath string
	Image                string
	ImagePullSecretName  string
	OwnerID              string `json:"OwnerID,omitempty"`
	ConnectionID         string
	Sync                 *syncRecord
	SocksListenAddr      string `json:"SocksListenAddr,omitempty"`
	SocksEgress          bool   `json:"SocksEgress,omitempty"`
}

type routeRecord struct {
	ExtraCIDR   []string
	ExtraDomain []string
	ExtraNodeIP bool
}

// syncRecord deliberately contains only the formerly exported configuration,
// never SyncOptions, its Kubernetes clients, lifecycle or runtime implementation.
type syncRecord struct {
	WorkloadNamespace    string
	ManagerNamespace     string
	Headers              map[string]string
	Workloads            []string
	ExtraRouteInfo       routeRecord
	TargetContainer      string
	TargetImage          string
	TargetWorkloadNames  map[string]string
	OriginKubeconfigPath string
	LocalDir             string
	RemoteDir            string
}

func recordRoute(route handler.ExtraRouteInfo) routeRecord {
	return routeRecord{ExtraCIDR: slices.Clone(route.ExtraCIDR), ExtraDomain: slices.Clone(route.ExtraDomain), ExtraNodeIP: route.ExtraNodeIP}
}

func recordConnection(c *handler.ConnectOptions) *connectionRecord {
	record := &connectionRecord{
		RequestRaw: slices.Clone(c.RequestRaw), ManagerNamespace: c.ManagerNamespace,
		WorkloadNamespace: c.WorkloadNamespace, ExtraRouteInfo: recordRoute(c.ExtraRouteInfo),
		OriginKubeconfigPath: c.OriginKubeconfigPath, Image: c.Image, ImagePullSecretName: c.ImagePullSecretName,
		OwnerID: c.OwnerID, ConnectionID: c.ConnectionID, SocksListenAddr: c.SocksListenAddr, SocksEgress: c.SocksEgress,
	}
	if s := c.GetSync(); s != nil {
		record.Sync = &syncRecord{
			WorkloadNamespace: s.WorkloadNamespace, ManagerNamespace: s.ManagerNamespace,
			Headers: maps.Clone(s.Headers), Workloads: slices.Clone(s.Workloads), ExtraRouteInfo: recordRoute(s.ExtraRouteInfo),
			TargetContainer: s.TargetContainer, TargetImage: s.TargetImage, TargetWorkloadNames: maps.Clone(s.TargetWorkloadNames),
			OriginKubeconfigPath: s.OriginKubeconfigPath, LocalDir: s.LocalDir, RemoteDir: s.RemoteDir,
		}
	}
	return record
}
