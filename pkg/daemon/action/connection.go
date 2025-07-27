package action

import (
	"context"
	"fmt"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

func (svr *Server) ConnectionList(ctx context.Context, req *rpc.ConnectionListRequest) (*rpc.ConnectionListResponse, error) {
	var list []*rpc.Status
	for _, options := range svr.connections {
		result := genStatus(options)
		list = append(list, result)
	}
	return &rpc.ConnectionListResponse{List: list, CurrentConnectionID: svr.currentConnectionID}, nil
}

func (svr *Server) ConnectionUse(ctx context.Context, req *rpc.ConnectionUseRequest) (*rpc.ConnectionUseResponse, error) {
	var index = -1
	for i, connection := range svr.connections {
		if connection.GetConnectionID() == req.ConnectionID {
			index = i
			break
		}
	}
	if index == -1 {
		return nil, fmt.Errorf("no connection found")
	}
	svr.currentConnectionID = req.ConnectionID
	return &rpc.ConnectionUseResponse{}, nil
}
