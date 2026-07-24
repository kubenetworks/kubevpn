package action

import (
	"context"
	"net"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

// siblingTunIPs returns the TUN IPs currently held by all established
// connections. A new connection excludes these from its own allocation so two
// clusters never hand out the same local TUN IP.
func (svr *Server) siblingTunIPs() []net.IP {
	svr.connMu.RLock()
	defer svr.connMu.RUnlock()
	var ips []net.IP
	for _, conn := range svr.connections {
		// GetLocalTunIP is data-plane-only (DataPlane). In the user daemon
		// connections are *ConnectOptions (not DataPlane), so the assertion fails
		// and contributes no IPs — correct, since sibling-IP reservation is a
		// root-daemon concern. Mirrors the old stub-returns-empty behavior.
		var v4, v6 string
		if dp, ok := conn.(handler.DataPlane); ok {
			v4, v6 = dp.GetLocalTunIP()
		}
		if ip := net.ParseIP(v4); ip != nil {
			ips = append(ips, ip)
		}
		if ip := net.ParseIP(v6); ip != nil {
			ips = append(ips, ip)
		}
	}
	return ips
}

// findConnection returns the first connection matching the given ID
// and its index. Returns (nil, -1) if not found.
// The caller must hold svr.connMu (at least RLock).
func (svr *Server) findConnection(connectionID string) (handler.Connection, int) {
	for i, conn := range svr.connections {
		if conn.GetConnectionID() == connectionID {
			return conn, i
		}
	}
	return nil, -1
}

// ConnectionList handles the ConnectionList RPC, returning the status of all active VPN connections.
func (svr *Server) ConnectionList(ctx context.Context, req *rpc.ConnectionListRequest) (*rpc.ConnectionListResponse, error) {
	svr.connMu.RLock()
	var ips map[string]tunIP
	if !svr.IsSudo {
		ips = svr.sudoHealthSnapshot(ctx)
	}
	var list []*rpc.Status
	for _, options := range svr.connections {
		result := buildConnectionStatus(options, ips)
		list = append(list, result)
	}
	currentID := svr.currentConnectionID
	svr.connMu.RUnlock()
	return &rpc.ConnectionListResponse{List: list, CurrentConnectionID: currentID}, nil
}

// removeConnection removes all connections matching the given ID from the
// slice and returns them. The caller is responsible for cleanup.
// The caller must hold svr.connMu (write lock).
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
// The caller must hold svr.connMu (write lock).
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
func cleanupConnection(ctx context.Context, conn handler.Connection) {
	if conn == nil {
		return
	}
	if sync := conn.GetSync(); sync != nil {
		_ = sync.Cleanup(ctx)
	}
	conn.Cleanup(ctx)
}

// ConnectionUse handles the ConnectionUse RPC, switching the active connection to the specified connection ID.
func (svr *Server) ConnectionUse(ctx context.Context, req *rpc.ConnectionUseRequest) (*rpc.ConnectionUseResponse, error) {
	svr.connMu.Lock()
	defer svr.connMu.Unlock()
	if _, i := svr.findConnection(req.ConnectionID); i == -1 {
		return nil, config.ErrConnectionNotFound
	}
	svr.currentConnectionID = req.ConnectionID
	return &rpc.ConnectionUseResponse{}, nil
}
