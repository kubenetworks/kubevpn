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
		result := buildConnectionStatus(options)
		list = append(list, result)
	}
	return &rpc.ConnectionListResponse{List: list, CurrentConnectionID: svr.currentConnectionID}, nil
}

// removeConnection removes all connections matching the given ID from the
// slice and returns them. The caller is responsible for cleanup.
func (svr *Server) removeConnection(connectionID string) handler.Connects {
	var removed handler.Connects
	for i := 0; i < len(svr.connections); i++ {
		if svr.connections[i].GetConnectionID() == connectionID {
			removed = removed.Append(svr.connections[i])
			svr.connections = append(svr.connections[:i], svr.connections[i+1:]...)
			i--
		}
	}
	return removed
}

// resetCurrentConnection updates currentConnectionID after the given ID was
// removed. It picks the first remaining connection, or clears the field.
func (svr *Server) resetCurrentConnection(removedID string) {
	if svr.currentConnectionID != removedID {
		return
	}
	svr.currentConnectionID = ""
	if len(svr.connections) > 0 {
		svr.currentConnectionID = svr.connections[0].GetConnectionID()
	}
}

// cleanupConnection cleans up a single connection's sync and VPN state.
func cleanupConnection(ctx context.Context, conn *handler.ConnectOptions) {
	if conn == nil {
		return
	}
	if conn.Sync != nil {
		_ = conn.Sync.Cleanup(ctx)
	}
	conn.Cleanup(ctx)
}

// ConnectionUse handles the ConnectionUse RPC, switching the active connection to the specified connection ID.
func (svr *Server) ConnectionUse(ctx context.Context, req *rpc.ConnectionUseRequest) (*rpc.ConnectionUseResponse, error) {
	if _, i := svr.findConnection(req.ConnectionID); i == -1 {
		return nil, fmt.Errorf("no connection found")
	}
	svr.currentConnectionID = req.ConnectionID
	return &rpc.ConnectionUseResponse{}, nil
}
