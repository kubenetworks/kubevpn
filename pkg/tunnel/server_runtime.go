package tunnel

// serverRuntime is the single composition root for server-side routing state.
// Embedding keeps the existing internal inspection points while making each
// component's ownership explicit.
type serverRuntime struct {
	*routeTable
	writer *routeWriter
	peers  *peerRegistry
	bridge *tunBridge
}

// Router is the public compatibility facade for the shared server runtime.
type Router struct {
	*serverRuntime
}

func NewRouter() *Router {
	table := &routeTable{routes: make(map[string]*ConnList)}
	writer := &routeWriter{table: table}
	peers := &peerRegistry{table: table, writer: writer, clients: make(map[string]*clientStack)}
	table.peers = peers
	return &Router{serverRuntime: &serverRuntime{
		routeTable: table,
		writer:     writer,
		peers:      peers,
		bridge:     newTUNBridge(),
	}}
}
