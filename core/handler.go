package core

import (
	"kubevpn/tun"
	"net"
)

// Handler is a proxy server handler
type Handler interface {
	Init(options ...HandlerOption)
	Handle(net.Conn)
}

// HandlerOptions describes the options for Handler.
type HandlerOptions struct {
	Chain    *Chain
	Node     Node
	IPRoutes []tun.IPRoute
}

// HandlerOption allows a common way to set handler options.
type HandlerOption func(opts *HandlerOptions)

// ChainHandlerOption sets the Chain option of HandlerOptions.
func ChainHandlerOption(chain *Chain) HandlerOption {
	return func(opts *HandlerOptions) {
		opts.Chain = chain
	}
}

// NodeHandlerOption set the server node for server handler.
func NodeHandlerOption(node Node) HandlerOption {
	return func(opts *HandlerOptions) {
		opts.Node = node
	}
}

// IPRoutesHandlerOption sets the IP routes for tun tunnel.
func IPRoutesHandlerOption(routes ...tun.IPRoute) HandlerOption {
	return func(opts *HandlerOptions) {
		opts.IPRoutes = routes
	}
}
