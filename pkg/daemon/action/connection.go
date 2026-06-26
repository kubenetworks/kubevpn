package action

import (
	"context"
	"fmt"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

// findConnection returns the first connection matching the given ID
// and its index. Returns (nil, -1) if not found.
func (svr *Server) findConnection(connectionID string) (*handler.ConnectOptions, int) {
	for i, conn := range svr.connections {
		if conn.GetConnectionID() == connectionID {
			return conn, i
		}
	}
	return nil, -1
}

// ConnectionList handles the ConnectionList RPC, returning the status of all active VPN connections.
func (svr *Server) ConnectionList(ctx context.Context, req *rpc.ConnectionListRequest) (*rpc.ConnectionListResponse, error) {
	var list []*rpc.Status
	for _, options := range svr.connections {
		result := genStatus(options)
		list = append(list, result)
	}
	return &rpc.ConnectionListResponse{List: list, CurrentConnectionID: svr.currentConnectionID}, nil
}

// ConnectionUse handles the ConnectionUse RPC, switching the active connection to the specified connection ID.
func (svr *Server) ConnectionUse(ctx context.Context, req *rpc.ConnectionUseRequest) (*rpc.ConnectionUseResponse, error) {
	if _, i := svr.findConnection(req.ConnectionID); i == -1 {
		return nil, fmt.Errorf("no connection found")
	}
	svr.currentConnectionID = req.ConnectionID
	return &rpc.ConnectionUseResponse{}, nil
}
