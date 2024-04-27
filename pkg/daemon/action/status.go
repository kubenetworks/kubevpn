package action

import (
	"context"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

const (
	StatusOk     = "Connected"
	StatusFailed = "Disconnected"

	ModeFull = "full"
	ModeLite = "lite"
)

func (svr *Server) Status(ctx context.Context, req *rpc.StatusRequest) (*rpc.StatusResponse, error) {
	var list []*rpc.Status

	if len(req.ClusterIDs) != 0 {
		for _, clusterID := range req.ClusterIDs {
			if svr.connect.GetClusterID() == clusterID {
				list = append(list, genStatus(svr.connect, ModeFull, 0))
			}
			for i, options := range svr.secondaryConnect {
				if options.GetClusterID() == clusterID {
					list = append(list, genStatus(options, ModeLite, int32(i+1)))
				}
			}
		}
	} else {
		if svr.connect != nil {
			list = append(list, genStatus(svr.connect, ModeFull, 0))
		}

		for i, options := range svr.secondaryConnect {
			list = append(list, genStatus(options, ModeLite, int32(i+1)))
		}
	}
	return &rpc.StatusResponse{List: list}, nil
}

func genStatus(connect *handler.ConnectOptions, mode string, index int32) *rpc.Status {
	status := StatusOk
	tunName, _ := connect.GetTunDeviceName()
	if tunName == "" {
		status = StatusFailed
	}
	info := rpc.Status{
		ID:         index,
		ClusterID:  connect.GetClusterID(),
		Cluster:    util.GetKubeconfigCluster(connect.GetFactory()),
		Mode:       mode,
		Kubeconfig: connect.OriginKubeconfigPath,
		Namespace:  connect.Namespace,
		Status:     status,
		Netif:      tunName,
	}
	return &info
}
